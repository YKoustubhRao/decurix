package httpapi

import (
	"context"
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
	if dest, ok := fileResp["default_dest"].(string); !ok || !strings.HasSuffix(dest, "sample.txt") {
		t.Errorf("expected default_dest ending in sample.txt, got %v", fileResp["default_dest"])
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
	if _, ok := dirResp["default_dest"].(string); !ok {
		t.Errorf("expected default_dest string, got %v", dirResp["default_dest"])
	}
}

func TestBrowseHostPathAnswersWithWhatWasPickedAndWhereItGoes(t *testing.T) {
	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	real := pickHostPaths
	defer func() { pickHostPaths = real }()

	var askedFolder bool
	pickHostPaths = func(_ context.Context, folder, _ bool) ([]string, error) {
		askedFolder = folder
		return []string{tmpDir, file, filepath.Join(tmpDir, "gone")}, nil
	}

	srv := &Server{}
	gated := http.NewServeMux()
	srv.RegisterRoutes(gated)
	primary := http.NewServeMux()
	srv.RegisterRoutes(primary)
	srv.RegisterLocalRoutes(primary)

	call := func(mux *http.ServeMux, method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/host-paths/browse?type=folder", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := call(gated, http.MethodPost); rec.Code != http.StatusNotFound {
		t.Fatalf("the gated listener must not open a chooser on the host, got %d", rec.Code)
	}
	if rec := call(primary, http.MethodGet); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("a GET must not open a chooser, got %d", rec.Code)
	}

	rec := call(primary, http.MethodPost)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool `json:"ok"`
		Paths []struct {
			Path  string `json:"path"`
			IsDir bool   `json:"is_dir"`
			Dest  string `json:"dest"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !askedFolder || !resp.OK || len(resp.Paths) != 2 {
		t.Fatalf("unexpected answer (folder=%v): %s", askedFolder, rec.Body.String())
	}
	if !resp.Paths[0].IsDir || resp.Paths[1].IsDir || !strings.HasSuffix(resp.Paths[1].Dest, "/notes.txt") ||
		!strings.HasPrefix(resp.Paths[1].Dest, "/home/headless/") {
		t.Fatalf("unexpected paths: %+v", resp.Paths)
	}

	pickHostPaths = func(context.Context, bool, bool) ([]string, error) { return nil, errNoHostPicker }
	rec = call(primary, http.MethodPost)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"unavailable":true`) {
		t.Fatalf("no chooser should be said plainly, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWindowsPickerIsToldWhichWindowToOpen(t *testing.T) {
	for _, c := range []struct {
		folder, either bool
		want           string
	}{
		{false, true, "$folder = $false\n$any = $true\n"},
		{false, false, "$folder = $false\n$any = $false\n"},
		{true, false, "$folder = $true\n$any = $false\n"},
	} {
		if got := windowsPickCommand("t", c.folder, c.either); !strings.HasPrefix(got, "$title = 't'\n"+c.want) {
			t.Errorf("folder=%v either=%v: script starts %q", c.folder, c.either, got[:60])
		}
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
