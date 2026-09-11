package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewCreatesRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	ws, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := os.Stat(ws.Root()); err != nil {
		t.Fatalf("root directory was not actually created: %v", err)
	}
}

func TestResolveSafeNormalPath(t *testing.T) {
	ws, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := ws.ResolveSafe("subdir/file.txt")
	if err != nil {
		t.Fatalf("ResolveSafe() error = %v", err)
	}
	want := filepath.Join(ws.Root(), "subdir", "file.txt")
	if got != want {
		t.Errorf("ResolveSafe() = %q, want %q", got, want)
	}
}

func TestResolveSafeRejectsTraversal(t *testing.T) {
	ws, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cases := []string{
		"../../../etc/passwd",
		"../outside.txt",
		"a/../../b",
	}
	for _, c := range cases {
		resolved, err := ws.ResolveSafe(c)
		if err != nil {
			continue // rejected outright - correct
		}
		// If it didn't error, it MUST still be confined to the root - this
		// is the real security property under test, not just "did it error".
		if !strings.HasPrefix(resolved, ws.Root()) {
			t.Errorf("ResolveSafe(%q) = %q, escaped root %q", c, resolved, ws.Root())
		}
	}
}

func TestResolveSafeRejectsAbsoluteEscape(t *testing.T) {
	ws, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// An absolute path outside the root must still resolve INSIDE the
	// root (treated as rooted at "/") or be rejected - never pass through
	// verbatim to somewhere else on disk.
	resolved, err := ws.ResolveSafe("/etc/passwd")
	if err == nil && !strings.HasPrefix(resolved, ws.Root()) {
		t.Errorf("ResolveSafe(\"/etc/passwd\") = %q, escaped root %q", resolved, ws.Root())
	}
}
