package httpapi

import (
	"archive/tar"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/YKoustubhRao/flowera/internal/desktop"
)

func TestDetectHostAppsHandler(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/host-apps/detect", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var items []desktop.HostAppItem
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}

	if len(items) == 0 {
		t.Fatalf("expected non-empty host app list")
	}

	foundBridge := false
	for _, it := range items {
		if it.ID == "host_bridge" {
			foundBridge = true
			if !it.Detected {
				t.Errorf("host_bridge should be detected")
			}
		}
	}
	if !foundBridge {
		t.Errorf("expected host_bridge in /api/host-apps/detect response")
	}

	// Verify ?refresh=1 force refresh parameter
	reqRef := httptest.NewRequest(http.MethodGet, "/api/host-apps/detect?refresh=1", nil)
	recRef := httptest.NewRecorder()
	mux.ServeHTTP(recRef, reqRef)
	if recRef.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on refresh, got %d: %s", recRef.Code, recRef.Body.String())
	}
}

func TestHostBridgeExecHandlerCLI(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	payload := `{"cmd": "echo", "args": ["hello-from-host-bridge"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/host-bridge/exec", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp hostBridgeExecResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Success {
		t.Errorf("expected success=true, got error=%s", resp.Error)
	}
	if !strings.Contains(resp.Stdout, "hello-from-host-bridge") {
		t.Errorf("expected output to contain hello-from-host-bridge, got %q", resp.Stdout)
	}
}

func TestHostBridgeExecHandlerValidation(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/host-bridge/exec", strings.NewReader(`{"cmd": ""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty cmd, got %d", rec.Code)
	}
}

func TestHostBridgeOpenHandlerValidation(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/host-bridge/open", strings.NewReader(`{"target": ""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty target, got %d", rec.Code)
	}
}

func TestFindDockerBinary(t *testing.T) {
	_ = findDockerBinary()
}

func TestCreateHostCmdWindowsCommand(t *testing.T) {
	cmd := createHostCmd(nil, "172.18.0.2:1", "cmd.exe", "/c", "start", "explorer.exe", "shell:AppsFolder\\OpenAI.Codex_2p2nqsd0c76g0!App")
	if cmd == nil {
		t.Fatal("expected non-nil cmd")
	}
	if len(cmd.Args) == 0 {
		t.Fatal("expected cmd to have args")
	}
}

// archiveDocker is a stand-in Docker daemon that reports one desktop and keeps the tar it is asked to extract into it.
func archiveDocker(t *testing.T, desktopID string) (*desktop.Manager, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	files := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/desktop_"+desktopID+"/json"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: desktopID}})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/"+desktopID+"/archive"):
			tr := tar.NewReader(r.Body)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(tr)
				mu.Lock()
				files[hdr.Name] = string(data)
				mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHTTPClient(ts.Client()), client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")), client.WithVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	return desktop.NewManager(cli), func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for k, v := range files {
			out[k] = v
		}
		return out
	}
}

func TestCopyHostAppsIntoAnExistingDesktop(t *testing.T) {
	mgr, written := archiveDocker(t, "work")
	srv := &Server{Desktops: mgr}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	post := func(form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/host-apps/copy", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("id=work&host_apps=host_bridge"); rec.Code != http.StatusOK {
		t.Fatalf("copy = %d: %s", rec.Code, rec.Body.String())
	}
	hostRun, ok := written()[".local/bin/host-run"]
	if !ok {
		t.Fatalf("the desktop was not sent host-run; it got %v", written())
	}
	if strings.Contains(hostRun, "curl -s") {
		t.Fatal("the refreshed host-run still calls curl")
	}

	if rec := post("id=nope&host_apps=host_bridge"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown desktop = %d, want 404", rec.Code)
	}
	if rec := post("id=work"); rec.Code != http.StatusBadRequest {
		t.Errorf("no apps = %d, want 400", rec.Code)
	}
	if rec := post("host_apps=host_bridge"); rec.Code != http.StatusBadRequest {
		t.Errorf("no id = %d, want 400", rec.Code)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/host-apps/copy", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
}
