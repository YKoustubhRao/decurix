package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestTakeTokenFileReadsAndDeletes checks the token is read, trimmed and its file deleted.
func TestTakeTokenFileReadsAndDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "soul.token")
	if err := os.WriteFile(path, []byte("abc123\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if got := takeTokenFile(path); got != "abc123" {
		t.Fatalf("token = %q, want abc123", got)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the token file is still there: %v", err)
	}
}

// TestTakeTokenFileRefusesWhatIsNotAFile checks a missing path, no path and a link give no token.
func TestTakeTokenFileRefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	if got := takeTokenFile(""); got != "" {
		t.Fatalf("no path gave %q", got)
	}
	if got := takeTokenFile(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("a missing file gave %q", got)
	}
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("not-yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "soul.token")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := takeTokenFile(link); got != "" {
		t.Fatalf("a link gave %q", got)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the link is still there: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the link's target was touched: %v", err)
	}
}
