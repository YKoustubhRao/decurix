package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sharedFolderRoot makes a stand-in for the host's C: drive holding Users/me/Documents/Shared, which the control
// plane sees through DECURIX_HOST_MNT_C.
func sharedFolderRoot(t *testing.T) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Users", "me", "Documents", "Shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECURIX_HOST_MNT_C", root)
}

func createWith(t *testing.T, handler http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return postForm(t, handler.ServeHTTP, "/api/desktops/create", form.Encode())
}

func TestCreateDesktopTakesOneValidatedSharedFolder(t *testing.T) {
	sharedFolderRoot(t)
	srv := &Server{Desktops: fakeDesktopManager(t)}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	rec := createWith(t, mux, url.Values{"name": {"share-ok"}, "profile": {"basic"}, "brain": {"ollama"},
		// The folder's own case: the stand-in drive is a case-sensitive Linux directory, where the real one (WSL's
		// /mnt/c) is not.
		"shared_folder": {`c:/Users/me/Documents/Shared/`}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a valid shared folder: %d %s", rec.Code, rec.Body.String())
	}
	var made map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil {
		t.Fatal(err)
	}
	// The card shows the folder as a person writes it, the case they typed kept for the names.
	if got, _ := made["shared_folder"].(string); !strings.EqualFold(got, `C:\users\me\documents\shared`) {
		t.Errorf("shared_folder = %q", got)
	}
	prov, ok := srv.Desktops.GetProvisioning("share-ok")
	if !ok || !strings.EqualFold(prov.SharedFolder, `C:\users\me\documents\shared`) {
		t.Errorf("the provisioning record does not say what is shared: %+v", prov)
	}

	// No shared folder is the default: nothing of the computer is named.
	rec = createWith(t, mux, url.Values{"name": {"share-none"}, "profile": {"basic"}, "brain": {"ollama"}})
	if rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), "shared_folder") {
		t.Errorf("a desktop with no shared folder: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateDesktopRefusesAnUnsafeSharedFolderInPlainWords(t *testing.T) {
	sharedFolderRoot(t)
	srv := &Server{Desktops: fakeDesktopManager(t)}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	for raw, want := range map[string]string{
		`C:\Windows`:                    "system folder",
		`C:\`:                           "whole drive",
		`C:\Users\me`:                   "whole home folder",
		`Documents\Shared`:              "full path",
		`C:\Users\me\Documents\Nowhere`: "does not exist",
		`\\server\share`:                "network folder",
	} {
		rec := createWith(t, mux, url.Values{"name": {"share-bad"}, "profile": {"basic"}, "brain": {"ollama"}, "shared_folder": {raw}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%q: %d %s, want 400 saying %q", raw, rec.Code, rec.Body.String(), want)
		}
		if _, made := srv.Desktops.GetProvisioning("share-bad"); made {
			t.Errorf("%q: a desktop was started for a folder that was refused", raw)
		}
	}
}

// A folder of the person's own computer is theirs to give at their own screen: the gated listener (the one a phone or a
// tunnel reaches with an access code) must never be able to hand one to a worker.
func TestTheGatedListenerCannotShareAFolderOfTheComputer(t *testing.T) {
	sharedFolderRoot(t)
	srv := &Server{Desktops: fakeDesktopManager(t)}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	gated := MarkGated(mux)

	rec := createWith(t, gated, url.Values{"name": {"share-lan"}, "profile": {"basic"}, "brain": {"ollama"},
		"shared_folder": {`C:\Users\me\Documents\Shared`}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only be chosen on this computer") {
		t.Fatalf("through the gated listener: %d %s", rec.Code, rec.Body.String())
	}
	if _, made := srv.Desktops.GetProvisioning("share-lan"); made {
		t.Fatal("a desktop was started with a shared folder through the gated listener")
	}

	// The same listener still makes an ordinary desktop.
	rec = createWith(t, gated, url.Values{"name": {"share-lan-plain"}, "profile": {"basic"}, "brain": {"ollama"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("an ordinary desktop through the gated listener: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOnlyTheServerCanMarkARequestAsGated(t *testing.T) {
	var seen []bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = append(seen, isGated(r)) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Decurix-Gated", "false")
	next.ServeHTTP(httptest.NewRecorder(), req)
	MarkGated(next).ServeHTTP(httptest.NewRecorder(), req)
	if len(seen) != 2 || seen[0] || !seen[1] {
		t.Fatalf("gated marks = %v", seen)
	}
}
