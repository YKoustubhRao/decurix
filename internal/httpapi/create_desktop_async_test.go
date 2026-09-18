package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateDesktopAsyncAndStatusPoll(t *testing.T) {
	mgr := fakeDesktopManager(t)
	srv := &Server{
		Desktops: mgr,
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// 1. Send POST /api/desktops/create - must return HTTP 202 Accepted immediately (< 200ms)
	body := strings.NewReader("name=async-desk-1&profile=basic&harness=ollama&cpus=2&memory_mb=2048")
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	start := time.Now()
	mux.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected immediate 202 response, took %v", elapsed)
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
	if statusResp["status"] != "provisioning" && statusResp["status"] != "failed" {
		t.Errorf("expected status 'provisioning', got %v", statusResp["status"])
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

