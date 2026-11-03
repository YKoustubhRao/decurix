package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// Copying a file of this computer into a new desktop is refused, a real file as much as a missing one: it reached the
// control plane's own keys and database (review 2026-11-02). A shared folder is the way to give a worker files.
func TestCreateDesktopRefusesHostPaths(t *testing.T) {
	mgr := fakeDesktopManager(t)
	srv := &Server{Desktops: mgr}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	validFile := filepath.Join(t.TempDir(), "repo.zip")
	if err := os.WriteFile(validFile, []byte("fake zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"host_paths", "copy_host_paths", "copy_paths"} {
		form := url.Values{"name": {"hp-" + strings.ReplaceAll(field, "_", "-")}, field: {validFile}}
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "shared folder") {
			t.Errorf("%s: got %d %s, want a refusal pointing at the shared folder", field, rec.Code, rec.Body.String())
		}
	}
}
