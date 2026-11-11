// healthz_test.go tests GET /healthz (what it says, primary listener only) and the drain at shutdown: a slow request
// in flight finishes, a stream ends at once, a new request is told to come back, and a drain out of budget ends what
// is left.
package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

// drainServer is a real listener behind lc.Wrap, as main.go builds them: /slow waits for release, /events is an
// event stream that runs until its context ends, /healthz is lc's.
func drainServer(t *testing.T, lc *Lifecycle, release <-chan struct{}, slowCtxDone chan<- error) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			io.WriteString(w, "finished")
		case <-r.Context().Done():
			slowCtxDone <- r.Context().Err()
		}
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc(healthzPath, lc.ServeHealth)
	srv := httptest.NewUnstartedServer(lc.Wrap(mux))
	srv.Config.BaseContext = lc.BaseContext
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// readHealth fetches /healthz from srv.
func readHealth(t *testing.T, srv *httptest.Server) healthReport {
	t.Helper()
	res, err := http.Get(srv.URL + healthzPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz answered %d", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var report healthReport
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestHealthzSaysWhichBinaryAnswered(t *testing.T) {
	lc := NewLifecycle("1.2.3")
	lc.Instance = "0123456789ab"
	srv := drainServer(t, lc, make(chan struct{}), make(chan error, 1))

	report := readHealth(t, srv)
	if report.Service != "decurix-control-plane" || report.Version != "1.2.3" || report.PID != os.Getpid() ||
		report.Instance != "0123456789ab" || report.Draining {
		t.Fatalf("health = %+v", report)
	}
	if started, err := time.Parse(time.RFC3339, report.StartedAt); err != nil || time.Since(started) > time.Minute {
		t.Fatalf("started_at = %q (%v)", report.StartedAt, err)
	}

	res, err := http.Post(srv.URL+healthzPath, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz = %d, want 405", res.StatusCode)
	}
}

func TestHealthzIsOnThePrimaryListenerOnly(t *testing.T) {
	s := &Server{}
	primary, gated := http.NewServeMux(), http.NewServeMux()
	s.RegisterLocalRoutes(primary)
	s.RegisterRoutes(gated)

	req := httptest.NewRequest(http.MethodGet, healthzPath, nil)
	if _, pattern := primary.Handler(req); pattern != healthzPath {
		t.Fatalf("primary routes /healthz to %q", pattern)
	}
	if _, pattern := gated.Handler(req); pattern == healthzPath {
		t.Fatal("the gated listener's routes include /healthz")
	}
}

func TestDrainLetsARequestInFlightFinishAndEndsAStreamAtOnce(t *testing.T) {
	lc := NewLifecycle("dev")
	release := make(chan struct{})
	slowCtxDone := make(chan error, 1)
	srv := drainServer(t, lc, release, slowCtxDone)

	// A slow request (a model call) in flight.
	slow := make(chan string, 1)
	go func() {
		res, err := http.Get(srv.URL + "/slow")
		if err != nil {
			slow <- "error: " + err.Error()
			return
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		slow <- strconv.Itoa(res.StatusCode) + " " + string(body)
	}()

	// An event stream, open and sending.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if line, _ := bufio.NewReader(stream.Body).ReadString('\n'); line != "data: hello\n" {
		t.Fatalf("stream said %q", line)
	}
	waitFor(t, 3*time.Second, "the slow request in flight", func() bool { lc.mu.Lock(); defer lc.mu.Unlock(); return lc.active == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drained := make(chan int, 1)
	go func() { drained <- lc.Drain(ctx) }()
	waitFor(t, 3*time.Second, "the drain to start", lc.Draining)

	// The stream ends at once, without waiting for the drain.
	streamEnded := make(chan struct{})
	go func() { io.Copy(io.Discard, stream.Body); close(streamEnded) }()
	select {
	case <-streamEnded:
	case <-time.After(3 * time.Second):
		t.Fatal("the event stream was still open during the drain")
	}

	// A new request is asked to come back; /healthz still answers, and says it drains.
	res, err := http.Get(srv.URL + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a new request during the drain got %d, want 503", res.StatusCode)
	}
	if retry, err := strconv.Atoi(res.Header.Get("Retry-After")); err != nil || retry < 1 || retry > maxRetryAfter {
		t.Fatalf("Retry-After = %q", res.Header.Get("Retry-After"))
	}
	if !readHealth(t, srv).Draining {
		t.Fatal("/healthz did not say draining")
	}

	// The drain waits for the slow request, which then finishes whole.
	select {
	case n := <-drained:
		t.Fatalf("the drain ended with the slow request still running (left %d)", n)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if got := <-slow; got != "200 finished" {
		t.Fatalf("the slow request ended %q", got)
	}
	select {
	case n := <-drained:
		if n != 0 {
			t.Fatalf("drain left %d", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the drain did not end once the slow request finished")
	}
}

func TestADrainOutOfBudgetEndsWhatIsLeft(t *testing.T) {
	lc := NewLifecycle("dev")
	slowCtxDone := make(chan error, 1)
	srv := drainServer(t, lc, make(chan struct{}), slowCtxDone)

	go func() {
		if res, err := http.Get(srv.URL + "/slow"); err == nil {
			res.Body.Close()
		}
	}()
	waitFor(t, 3*time.Second, "the slow request in flight", func() bool { lc.mu.Lock(); defer lc.mu.Unlock(); return lc.active == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if left := lc.Drain(ctx); left != 1 {
		t.Fatalf("drain left %d, want 1", left)
	}
	lc.Abort()
	select {
	case err := <-slowCtxDone:
		if err == nil {
			t.Fatal("the request's context ended without an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Abort did not end the request still running")
	}
}

func TestLongLivedRequests(t *testing.T) {
	for _, c := range []struct {
		path, header, value string
		want                bool
	}{
		{"/desktop/abc/websockify", "Upgrade", "websocket", true},
		{"/api/phones/connect", "Upgrade", "WebSocket", true},
		{"/api/desktops/chat/stream", "", "", true},
		{"/api/anything", "Accept", "text/event-stream", true},
		{"/api/llm/azure/v1/chat/completions", "", "", false},
		{"/api/desktops", "Accept", "application/json", false},
	} {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.header != "" {
			r.Header.Set(c.header, c.value)
		}
		if got := longLived(r); got != c.want {
			t.Errorf("longLived(%s %s=%s) = %v, want %v", c.path, c.header, c.value, got, c.want)
		}
	}
}

func TestDrainBudget(t *testing.T) {
	for _, c := range []struct {
		raw      string
		want     time.Duration
		warnings bool
	}{
		{"", DefaultDrainBudget, false},
		{"45", 45 * time.Second, false},
		{"2m", 2 * time.Minute, false},
		{"0", 0, false},
		{"-5", DefaultDrainBudget, true},
		{"soon", DefaultDrainBudget, true},
	} {
		got, note := DrainBudget(c.raw)
		if got != c.want || (note != "") != c.warnings {
			t.Errorf("DrainBudget(%q) = %s, %q", c.raw, got, note)
		}
	}
}

func TestRetryAfterIsWhatIsLeftOfTheDrainWithinBounds(t *testing.T) {
	lc := NewLifecycle("dev")
	now := time.Now()
	if got := lc.retryAfterLocked(now); got != 5 {
		t.Errorf("no deadline: %d", got)
	}
	for _, c := range []struct {
		left time.Duration
		want int
	}{{90 * time.Second, maxRetryAfter}, {10 * time.Second, 10}, {-time.Second, 1}} {
		lc.deadline = now.Add(c.left)
		if got := lc.retryAfterLocked(now); got != c.want {
			t.Errorf("%s left: Retry-After %d, want %d", c.left, got, c.want)
		}
	}
}
