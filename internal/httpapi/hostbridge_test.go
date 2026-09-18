package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
