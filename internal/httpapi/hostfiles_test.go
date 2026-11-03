package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

func TestHostFileStatAnswersOnlyForTheOpenTasksOwnFiles(t *testing.T) {
	drive := t.TempDir()
	if err := os.MkdirAll(filepath.Join(drive, "Users", "me", "Documents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "Users", "me", "Documents", "report.xlsx"), []byte("PK\x03\x04."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := hostCDrives
	hostCDrives = func() []string { return []string{drive} }
	t.Cleanup(func() { hostCDrives = old })

	s := chatServer(t)
	request := `Open C:\Users\me\Documents\sales.csv in Excel (Host), and save it as report.xlsx in C:\Users\me\Documents.`
	route, err := s.Tasks.Plan("office", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.Commit(route, tasks.Worker{ID: "office"}, request); err != nil {
		t.Fatal(err)
	}

	stat := func(desktop, path string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		q := url.Values{"desktop": {desktop}, "path": {path}}
		s.hostFileStatHandler(rec, httptest.NewRequest(http.MethodGet, "/api/host-files/stat?"+q.Encode(), nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	if code, body := stat("office", `C:\Users\me\Documents\report.xlsx`); code != 200 || body["exists"] != true || body["size"] != float64(5) || body["kind"] != "zip" {
		t.Fatalf("the saved file: %d %v", code, body)
	}
	if code, body := stat("office", `C:\Users\me\Documents\sales.csv`); code != 200 || body["exists"] != false {
		t.Fatalf("a named file that is not there: %d %v", code, body)
	}
	read := func(desktopID, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		q := url.Values{"desktop": {desktopID}, "path": {path}}
		s.hostFileReadHandler(rec, httptest.NewRequest(http.MethodGet, "/api/host-files/read?"+q.Encode(), nil))
		return rec
	}
	if rec := read("office", `C:\Users\me\Documents\report.xlsx`); rec.Code != 200 || rec.Body.String() != "PK\x03\x04." {
		t.Fatalf("reading the saved file: %d %q", rec.Code, rec.Body.String())
	}
	if rec := read("office", `C:\secret.txt`); rec.Code != http.StatusForbidden {
		t.Fatalf("a file the request does not name must not be read, got %d", rec.Code)
	}
	if code, _ := stat("office", `C:\secret.txt`); code != http.StatusForbidden {
		t.Fatalf("a file the request does not name must not be answered, got %d", code)
	}
	if code, _ := stat("other", `C:\Users\me\Documents\report.xlsx`); code != http.StatusForbidden {
		t.Fatalf("another desktop's task must not open it, got %d", code)
	}
	if code, _ := stat("office", `C:\Users\me\Documents\..\..\..\secret.txt`); code != http.StatusBadRequest {
		t.Fatalf("a path going up must be refused, got %d", code)
	}
}

// Review 2026-11-02: the deny-list was decided on the path as written, and the "Application Data" and "Local
// Settings" junctions, a trailing dot or space, an 8.3 short name and any symlink or junction led around it into
// AppData - with the path only having to be named in a task, which can come from an email. The path is decided on
// as it resolves now, and only documents are read.
func TestHostFilesDecideOnTheResolvedPath(t *testing.T) {
	drive := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(drive, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Users/me/Documents/report.xlsx", "PK\x03\x04.")
	write("Users/me/Documents/id_rsa", "KEY")
	write("Users/me/AppData/Roaming/Browser/secrets.json", `{"password":"x"}`)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "elsewhere.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := hostCDrives
	hostCDrives = func() []string { return []string{drive} }
	t.Cleanup(func() { hostCDrives = old })

	// link makes a symlink, or on Windows without the right a junction; it reports whether either could be made.
	link := func(target, name string) bool {
		t.Helper()
		at := filepath.Join(drive, filepath.FromSlash(name))
		if err := os.Symlink(target, at); err == nil {
			return true
		}
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "mklink", "/J", at, target).Run() == nil
		}
		return false
	}
	links := link(filepath.Join(drive, "Users", "me", "AppData", "Roaming"), "Users/me/Documents/notes") &&
		link(filepath.Join(drive, "Users", "me", "Documents"), "Users/me/My Docs") &&
		link(outside, "Users/me/Documents/out")

	s := chatServer(t)
	request := `Check C:\Users\me\Documents\report.xlsx, C:\Users\me\Documents\notes\Browser\secrets.json, ` +
		`C:\Users\me\My Docs\report.xlsx, C:\Users\me\Documents\out\elsewhere.txt, C:\Users\me\AppData.\Roaming\Browser\secrets.json, ` +
		`C:\Users\me\AppData \Roaming\Browser\secrets.json, C:\Users\me\APPDAT~1\Roaming\Browser\secrets.json, ` +
		`C:\Users\me\appdata\Roaming\Browser\secrets.json, C:\Users\me\Application Data\Browser\secrets.json, ` +
		`C:\Users\me\Local Settings\secrets.json, C:\Users\me\Documents\id_rsa, C:\users\ME\documents\REPORT.xlsx, ` +
		`C:\Users\me\Documents\report.xlsx:hidden, C:\Users\me\Documents\CON.txt.`
	route, err := s.Tasks.Plan("office", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.Commit(route, tasks.Worker{ID: "office"}, request); err != nil {
		t.Fatal(err)
	}
	read := func(path string) int {
		rec := httptest.NewRecorder()
		q := url.Values{"desktop": {"office"}, "path": {path}}
		s.hostFileReadHandler(rec, httptest.NewRequest(http.MethodGet, "/api/host-files/read?"+q.Encode(), nil))
		return rec.Code
	}
	stat := func(path string) int {
		rec := httptest.NewRecorder()
		q := url.Values{"desktop": {"office"}, "path": {path}}
		s.hostFileStatHandler(rec, httptest.NewRequest(http.MethodGet, "/api/host-files/stat?"+q.Encode(), nil))
		return rec.Code
	}
	for path, want := range map[string]int{
		`C:\Users\me\Documents\report.xlsx`:                 http.StatusOK,
		`C:\users\ME\documents\REPORT.xlsx`:                 http.StatusOK, // Windows ignores case; the listed names are used
		`C:\Users\me\AppData.\Roaming\Browser\secrets.json`: http.StatusForbidden,
		`C:\Users\me\AppData \Roaming\Browser\secrets.json`: http.StatusForbidden,
		`C:\Users\me\APPDAT~1\Roaming\Browser\secrets.json`: http.StatusForbidden,
		`C:\Users\me\appdata\Roaming\Browser\secrets.json`:  http.StatusForbidden,
		`C:\Users\me\Application Data\Browser\secrets.json`: http.StatusForbidden,
		`C:\Users\me\Local Settings\secrets.json`:           http.StatusForbidden,
		`C:\Users\me\Documents\id_rsa`:                      http.StatusForbidden, // no document
		`C:\Users\me\Documents\report.xlsx:hidden`:          http.StatusForbidden, // a stream
		`C:\Users\me\Documents\CON.txt.`:                    http.StatusForbidden,
	} {
		if got := read(path); got != want {
			t.Errorf("read %s: got %d, want %d", path, got, want)
		}
	}
	if code := stat(`C:\Users\me\APPDAT~1\Roaming\Browser\secrets.json`); code != http.StatusForbidden {
		t.Errorf("stat through a short name: %d", code)
	}
	if !links {
		t.Skip("this machine can make neither symlinks nor junctions here")
	}
	for path, want := range map[string]int{
		`C:\Users\me\Documents\notes\Browser\secrets.json`: http.StatusForbidden, // a link into AppData
		`C:\Users\me\Documents\out\elsewhere.txt`:          http.StatusForbidden, // a link out of the drive
		`C:\Users\me\My Docs\report.xlsx`:                  http.StatusOK,        // a link to an ordinary folder
	} {
		if got := read(path); got != want {
			t.Errorf("read %s: got %d, want %d", path, got, want)
		}
	}
	if code := stat(`C:\Users\me\Documents\notes\Browser\secrets.json`); code != http.StatusForbidden {
		t.Errorf("stat through a link into AppData: %d", code)
	}
	if runtime.GOOS != "windows" {
		return
	}
	// The junctions Windows itself makes ("Application Data") are junctions, not symlinks: Go reports them as
	// irregular and EvalSymlinks does not follow them.
	junction := func(target, name string) {
		t.Helper()
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(drive, filepath.FromSlash(name)), target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v %s", err, out)
		}
	}
	junction(filepath.Join(drive, "Users", "me", "AppData", "Roaming"), "Users/me/Documents/jroaming")
	junction(filepath.Join(drive, "Users", "me", "Documents"), "Users/me/JDocs")
	if err := s.Tasks.CloseOpen("office", ""); err != nil {
		t.Fatal(err)
	}
	request2 := `Check C:\Users\me\Documents\jroaming\Browser\secrets.json and C:\Users\me\JDocs\report.xlsx.`
	route2, err := s.Tasks.Plan("office", request2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.Commit(route2, tasks.Worker{ID: "office"}, request2); err != nil {
		t.Fatal(err)
	}
	if got := read(`C:\Users\me\Documents\jroaming\Browser\secrets.json`); got != http.StatusForbidden {
		t.Errorf("read through a junction into AppData: %d", got)
	}
	if got := read(`C:\Users\me\JDocs\report.xlsx`); got != http.StatusOK {
		t.Errorf("read through a junction to an ordinary folder: %d", got)
	}
}
