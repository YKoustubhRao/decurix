package trace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A stored trace keeps how many figures a check found, never the figures;
// files older than KeepDays are deleted.
func TestStoredTracesHoldCountsAndRotate(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, time.Now().UTC().AddDate(0, 0, -KeepDays-2).Format("2006-01-02")+".jsonl")
	os.WriteFile(old, []byte("{}\n"), 0o600)
	s := NewStore(dir)
	r := &Record{ID: "a", At: time.Now().UTC(), Checks: []Check{{Kind: "unsupported_figures", Detail: "figures", Items: []string{"4,50,000", "18,00,000"}}}}
	s.Add(r)
	if len(r.Checks[0].Items) != 2 {
		t.Fatal("the caller's record lost its items")
	}
	got, ok := s.Get("a")
	if !ok || len(got.Checks[0].Items) != 0 || !strings.Contains(got.Checks[0].Detail, "(2)") {
		t.Fatalf("stored %+v", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, r.At.Format("2006-01-02")+".jsonl"))
	if strings.Contains(string(data), "4,50,000") {
		t.Fatalf("a figure reached the trace file: %s", data)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("an old trace file was kept")
	}
}

// The trace folder is owner-only like its files.
func TestModeTraceDirIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores Unix file modes")
	}
	dir := filepath.Join(t.TempDir(), "traces")
	NewStore(dir).Add(&Record{ID: "a", At: time.Now().UTC()})
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("traces dir %o, want 700", st.Mode().Perm())
	}
}
