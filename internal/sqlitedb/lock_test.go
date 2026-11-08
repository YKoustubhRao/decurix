package sqlitedb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two control planes on one data directory: the second is refused with a message naming the first, and once the first
// lets go the directory can be taken again.
func TestLockDataDirIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	first, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	second, err := LockDataDir(dir)
	if err == nil {
		_ = second.Release()
		t.Fatal("a second lock on the same data directory was taken")
	}
	if !errors.Is(err, ErrDataDirInUse) || !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) ||
		!strings.Contains(err.Error(), dir) {
		t.Fatalf("the refusal does not name the holder and the folder: %v", err)
	}
	// Another data directory is not held.
	other, err := LockDataDir(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatalf("another data directory: %v", err)
	}
	_ = other.Release()

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("a second release: %v", err)
	}
}
