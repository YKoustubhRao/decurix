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
