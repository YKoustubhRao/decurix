package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
