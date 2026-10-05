package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHostPathEndpoint(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// 1. Missing path parameter -> 400
	req := httptest.NewRequest(http.MethodGet, "/api/host-paths/validate", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing path, got %d", rec.Code)
	}

	// 2. Non-existent path -> exists: false
	req = httptest.NewRequest(http.MethodGet, "/api/host-paths/validate?path="+url.QueryEscape("/non/existent/path/here"), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["exists"] != false || resp["valid"] != false {
		t.Errorf("expected exists=false, got %v", resp)
	}

	// 3. Existing file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(testFile, []byte("abc 123"), 0o644); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/host-paths/validate?path="+url.QueryEscape(testFile), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var fileResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fileResp); err != nil {
		t.Fatal(err)
	}
	if fileResp["exists"] != true || fileResp["is_dir"] != false || fileResp["name"] != "sample.txt" {
		t.Errorf("unexpected response for file: %+v", fileResp)
	}

	// 4. Existing directory
	req = httptest.NewRequest(http.MethodGet, "/api/host-paths/validate?path="+url.QueryEscape(tmpDir), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var dirResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &dirResp); err != nil {
		t.Fatal(err)
	}
	if dirResp["exists"] != true || dirResp["is_dir"] != true {
		t.Errorf("unexpected response for dir: %+v", dirResp)
	}
}

func TestCreateDesktopWithHostPaths(t *testing.T) {
	mgr := fakeDesktopManager(t)
	srv := &Server{
		Desktops: mgr,
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	tmpDir := t.TempDir()
	validFile := filepath.Join(tmpDir, "repo.zip")
	if err := os.WriteFile(validFile, []byte("fake zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Test 1: Non-existent host path should fail with 400 Bad Request
	form := url.Values{
		"name":       {"test-bad-path"},
		"profile":    {"basic"},
		"brain":      {"ollama"},
		"host_paths": {"/completely/missing/file.txt -> Desktop/file.txt"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-existent host path, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "host path does not exist") {
		t.Errorf("expected error message to mention path does not exist, got: %s", rec.Body.String())
	}

	// Test 2: Destination escaping /home/headless should fail with 400 Bad Request
	form = url.Values{
		"name":       {"test-traversal"},
		"profile":    {"basic"},
		"brain":      {"ollama"},
		"host_paths": {fmt.Sprintf("%s -> /etc/shadow", validFile)},
	}
	req = httptest.NewRequest(http.MethodPost, "/api/desktops/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for path traversal, got %d: %s", rec.Code, rec.Body.String())
	}

	// Test 3: Valid file path should succeed and return 202 Accepted
	form = url.Values{
		"name":       {"test-valid-path"},
		"profile":    {"basic"},
		"brain":      {"ollama"},
		"host_paths": {fmt.Sprintf(`[{"src":%q,"dest":"Desktop/repo.zip"}]`, validFile)},
	}
	req = httptest.NewRequest(http.MethodPost, "/api/desktops/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted for valid host path, got %d: %s", rec.Code, rec.Body.String())
	}
}
