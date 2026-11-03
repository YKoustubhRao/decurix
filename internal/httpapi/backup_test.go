package httpapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/claudeauth"
	"github.com/YKoustubhRao/decurix/internal/llm"
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

// A backup holds a snapshot that opens as a database with the rows in it and a
// manifest - and never the live WAL files, and never a key: a stolen backup
// must not open the saved secrets (review 2026-11-02 found it asserting the
// opposite, that llm.key was in it).
func TestBackupHoldsASnapshotAndNoKeys(t *testing.T) {
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

	// Every kind of key file the data dir holds, and one that is not a key.
	secretFiles := map[string]string{
		"llm.key": "llm-key-bytes", "mail.key": "mail-key-bytes", "cloudflare.key": "cf-key-bytes",
		"hostgui.key": "hostgui-key-bytes", QualificationOwnerFile: "owner-token-bytes", "some-future.token": "future-token-bytes",
	}
	for name, body := range secretFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, keptHostAppsFile), []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A model key saved sealed, one saved unsealed (a key file that could not
	// be written), and the Claude Code login, which is always kept as it is.
	sealKey := make([]byte, 32)
	sealedHub, err := llm.NewHub(db, sealKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := sealedHub.Save(llm.ProviderClaude, llm.Settings{Key: "sk-ant-sealed-secret"}); err != nil {
		t.Fatal(err)
	}
	plainHub, err := llm.NewHub(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plainHub.Save(llm.ProviderAzure, llm.Settings{Endpoint: "https://mine.services.ai.azure.com/models", Key: "azure-plain-secret"}); err != nil {
		t.Fatal(err)
	}
	logins, err := claudeauth.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := logins.Save(`{"claudeAiOauth":{"accessToken":"oauth-plain-secret"}}`, "me@example.com"); err != nil {
		t.Fatal(err)
	}

	s := &Server{DB: db, Desktops: fakeDesktopManager(t)}
	rec := httptest.NewRecorder()
	s.backupHandler(rec, httptest.NewRequest(http.MethodGet, "/api/backup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	files := readBackup(t, rec.Body)
	for _, want := range []string{"manifest.json", "data/" + sqlitedb.FileName, "data/" + keptHostAppsFile, "problems.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s missing; got %v", want, keys(files))
		}
	}
	for name := range files {
		if name == "data/"+sqlitedb.FileName+"-wal" || name == "data/"+sqlitedb.FileName+"-shm" {
			t.Errorf("%s is in the backup: the snapshot covers it", name)
		}
	}
	for name, body := range secretFiles {
		if _, ok := files["data/"+name]; ok {
			t.Errorf("data/%s is in the backup: whoever holds it can open the saved secrets", name)
		}
		for entry, b := range files {
			if bytes.Contains(b, []byte(body)) {
				t.Errorf("the bytes of %s are in %s", name, entry)
			}
		}
	}

	var man backupManifest
	if err := json.Unmarshal(files["manifest.json"], &man); err != nil || man.Version != 1 {
		t.Fatalf("manifest %s: %v", files["manifest.json"], err)
	}
	if len(man.SecretsLeftOut) == 0 {
		t.Error("the manifest does not say what the restore has to have entered again")
	}

	// Nothing that opens without a key file is anywhere in the snapshot's
	// bytes, freed pages included.
	snapBytes := files["data/"+sqlitedb.FileName]
	for _, secret := range []string{"azure-plain-secret", "oauth-plain-secret", "sk-ant-sealed-secret"} {
		if bytes.Contains(snapBytes, []byte(secret)) {
			t.Errorf("%q is readable in the snapshot", secret)
		}
	}

	snap := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(snap, snapBytes, 0o600); err != nil {
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
	var loginRows int
	if err := restored.QueryRow(`SELECT COUNT(*) FROM claude_session`).Scan(&loginRows); err != nil || loginRows != 0 {
		t.Errorf("claude_session rows = %d, %v: the login went into the backup", loginRows, err)
	}

	// The sealed key stays sealed (it opens again on the same install, whose
	// llm.key is still there); the unsealed one is gone.
	back, err := llm.NewHub(restored, sealKey)
	if err != nil {
		t.Fatal(err)
	}
	st := back.Status()
	if !st[llm.ProviderClaude].Configured {
		t.Error("the sealed Claude key did not open again with the install's own key file")
	}
	if st[llm.ProviderAzure].Configured {
		t.Error("the Azure key saved unsealed came back from the backup")
	}
	otherKey := make([]byte, 32)
	otherKey[0] = 1
	elsewhere, err := llm.NewHub(restored, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere.Status()[llm.ProviderClaude].Configured {
		t.Error("the sealed key opened with another install's key")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
