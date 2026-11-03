package sqlitedb

import (
	"os"
	"path/filepath"
	"testing"
)

// Two data directories get two instance IDs - even ones whose path reads the same in two containers - and one data
// directory keeps its ID across restarts (RW1-C-03).
func TestEachDataDirectoryKeepsItsOwnInstanceID(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	first := instanceIDIn(a)
	if len(first) != 12 || instanceIDIn(a) != first {
		t.Fatalf("id %q is not kept", first)
	}
	if instanceIDIn(b) == first {
		t.Fatal("two data directories share an instance id")
	}
	// The same path string with a different file (another container's /data) is another instance.
	if err := os.WriteFile(filepath.Join(a, InstanceFile), []byte("0123456789ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := instanceIDIn(a); got != "0123456789ab" {
		t.Fatalf("read %q", got)
	}
}

// Many starts at once on one data directory agree on one ID, and the file holds just it (an exclusive create, not a
// hard link, so it works on any filesystem; REVIEW-3 follow-up 8).
func TestConcurrentStartsAgreeOnOneInstanceID(t *testing.T) {
	dir := t.TempDir()
	ids := make(chan string, 8)
	for i := 0; i < cap(ids); i++ {
		go func() { ids <- instanceIDIn(dir) }()
	}
	first := <-ids
	for i := 1; i < cap(ids); i++ {
		if got := <-ids; got != first {
			t.Fatalf("two starts made two ids: %q and %q", first, got)
		}
	}
	if got, ok := readInstanceID(filepath.Join(dir, InstanceFile)); !ok || got != first {
		t.Fatalf("the file holds %q, want %q", got, first)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("only the instance file is left in the data directory: %v", entries)
	}
}
