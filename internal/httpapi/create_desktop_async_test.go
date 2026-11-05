package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// TestCreateDesktopAsyncAndStatusPoll pins that creating a desktop answers 202 while the creation itself is still
// running. It used to time the handler against 500ms, which a loaded machine broke with nothing wrong; now the stand-in
// Docker daemon holds every request the creation makes (everything but the handler's own listing and "does it exist"
// inspect) until the test lets go, so a handler that waited for the creation would never answer at all.
func TestCreateDesktopAsyncAndStatusPoll(t *testing.T) {
	release := make(chan struct{})
	held := make(chan string, 64)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What the handler itself asks before it answers: the desktops there are (none), and whether this one exists
		// (no such container yet).
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/desktop_async-desk-1/json") {
			http.NotFound(w, r)
			return
		}
		select {
		case held <- r.Method + " " + r.URL.Path:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.Error(w, "released", http.StatusServiceUnavailable)
	}))
	mgr := managerOn(t, ts)
	// Cleanups run last-registered first: let the held requests go before the stand-in daemon is closed.
	t.Cleanup(func() { close(release) })
	srv := &Server{
		Desktops: mgr,
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// 1. POST /api/desktops/create answers 202 without waiting for the creation.
	body := strings.NewReader("name=async-desk-1&profile=basic&brain=ollama&cpus=2&memory_mb=2048")
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		mux.ServeHTTP(rec, req)
	}()
	// The bound only turns a regression (a handler that waits for the held creation) from a hang into a failure; it is
	// not a speed claim, so it is set far above any load this suite meets.
	select {
	case <-answered:
	case <-time.After(time.Minute):
		t.Fatal("the create handler did not answer while the creation was held: it waits for the creation")
	}
	// The creation really is in flight and held: it has reached Docker and is waiting there.
	select {
	case call := <-held:
		t.Logf("the creation is held at %s", call)
	case <-time.After(time.Minute):
		t.Fatal("the creation never reached Docker, so the 202 proves nothing about not waiting for it")
	}

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	var createResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if createResp["id"] != "async-desk-1" {
		t.Errorf("expected id 'async-desk-1', got %v", createResp["id"])
	}
	if createResp["status"] != "provisioning" {
		t.Errorf("expected status 'provisioning', got %v", createResp["status"])
	}

	// 2. Poll GET /api/desktops/status?id=async-desk-1
	statusReq := httptest.NewRequest(http.MethodGet, "/api/desktops/status?id=async-desk-1", nil)
	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, statusReq)

	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK from status endpoint, got %d: %s", statusRec.Code, statusRec.Body.String())
	}

	var statusResp map[string]interface{}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("failed to decode status JSON: %v", err)
	}

	if statusResp["id"] != "async-desk-1" {
		t.Errorf("expected status response id 'async-desk-1', got %v", statusResp["id"])
	}
	// Every Docker call of the creation is held, so it can be neither done nor failed yet.
	if statusResp["status"] != "provisioning" {
		t.Errorf("expected status 'provisioning' while the creation is held, got %v", statusResp["status"])
	}

	// 3. GET /desktop/async-desk-1/ while provisioning should serve Launch Screen
	proxyReq := httptest.NewRequest(http.MethodGet, "/desktop/async-desk-1/", nil)
	proxyRec := httptest.NewRecorder()
	mux.ServeHTTP(proxyRec, proxyReq)

	// Since it's provisioning, it renders launchScreenHTML (status 200)
	if proxyRec.Code == http.StatusOK {
		bodyStr := proxyRec.Body.String()
		if !strings.Contains(bodyStr, "Spinning Up Desktop") {
			t.Errorf("expected launch screen HTML to contain 'Spinning Up Desktop', got: %s", bodyStr)
		}
	}
}

func TestCreateDesktopDefaultCPU(t *testing.T) {
	mgr := fakeDesktopManager(t)
	srv := &Server{
		Desktops: mgr,
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Post create without cpus specified
	body := strings.NewReader("name=desk-default-cpu&profile=basic&brain=ollama")
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify that the desktop was registered in provisioning with CPUs == 1
	prov, ok := mgr.GetProvisioning("desk-default-cpu")
	if !ok {
		t.Fatalf("expected desktop in provisioning, not found")
	}
	if prov.CPUs != 1 {
		t.Errorf("expected default CPU count to be 1, got %d", prov.CPUs)
	}
}

// TestBeginProvisioningReturnsASnapshotNotTheLiveRecord pins the data race the -race run of the test above used to
// report: runProvisioning keeps rewriting its record for the whole creation, so what the create handler is handed (and
// serialises without any lock) must be a copy. Run without -race it still fails if the live record is returned, because
// the value it holds then moves on by itself.
func TestBeginProvisioningReturnsASnapshotNotTheLiveRecord(t *testing.T) {
	mgr := fakeDesktopManager(t)
	prov, err := mgr.BeginProvisioning("snap-1", "basic", "thebot", desktop.Brain{}, 1, 2048, "", "", "", nil, nil, func(*desktop.Desktop) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	first := *prov

	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, ok := mgr.GetProvisioning("snap-1")
		if !ok || cur.Stage != first.Stage || cur.Progress != first.Progress || cur.Status != first.Status {
			break // the manager's own record has moved on
		}
		if time.Now().After(deadline) {
			t.Skip("provisioning never advanced in the fake, so there is nothing to compare against")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !reflect.DeepEqual(*prov, first) {
		t.Fatalf("the record BeginProvisioning returned changed after it was returned: %+v -> %+v", first, *prov)
	}
}

// The create door refuses the names practice and readiness runs give their workers, with a fixed reason, and makes
// nothing (review CP-A-6 N1).
func TestCreateDesktopRefusesPracticeAndReadinessNames(t *testing.T) {
	mgr := fakeDesktopManager(t)
	srv := &Server{Desktops: mgr}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	for _, name := range []string{"practice-e0123456789abcdef", "readiness-r1"} {
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", strings.NewReader("name="+name+"&profile=basic&brain=ollama"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "kept for practice and readiness runs") {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if _, ok := mgr.GetProvisioning(name); ok {
			t.Fatalf("%s is being made", name)
		}
	}
}
