package httpapi

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// readBackup returns every file in a backup archive by name.
func readBackup(t *testing.T, body io.Reader) map[string][]byte {
	t.Helper()

	gz, err := gzip.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = b
	}
}

// A backup holds a snapshot that opens as a database with the rows in it,
// the data dir's key files, and a manifest - and never the live WAL files.
func TestBackupHoldsASnapshotAndTheKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DECURIX_DATA_DIR", dir)

	db, err := sqlitedb.Open(filepath.Join(dir, sqlitedb.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE lesson (what TEXT); INSERT INTO lesson VALUES ('file the invoice under the PO')`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "llm.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &Server{DB: db, Desktops: fakeDesktopManager(t)}
	rec := httptest.NewRecorder()
	s.backupHandler(rec, httptest.NewRequest(http.MethodGet, "/api/backup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	files := readBackup(t, rec.Body)
	for _, want := range []string{"manifest.json", "data/" + sqlitedb.FileName, "data/llm.key", "problems.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s missing; got %v", want, keys(files))
		}
	}
	for name := range files {
		if name == "data/"+sqlitedb.FileName+"-wal" || name == "data/"+sqlitedb.FileName+"-shm" {
			t.Errorf("%s is in the backup: the snapshot covers it", name)
		}
	}

	var man backupManifest
	if err := json.Unmarshal(files["manifest.json"], &man); err != nil || man.Version != 1 {
		t.Fatalf("manifest %s: %v", files["manifest.json"], err)
	}

	snap := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(snap, files["data/"+sqlitedb.FileName], 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := sql.Open("sqlite", snap)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var what string
	if err := restored.QueryRow(`SELECT what FROM lesson`).Scan(&what); err != nil || what != "file the invoice under the PO" {
		t.Fatalf("snapshot row = %q, %v", what, err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
