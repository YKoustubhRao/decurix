package sealed

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key, err := LoadKey(t.TempDir(), "test.key")
	if err != nil {
		t.Fatal(err)
	}

	stored, err := Seal(key, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(stored) || strings.Contains(stored, "hunter2") {
		t.Fatalf("stored = %q", stored)
	}

	plain, err := Open(key, stored)
	if err != nil || plain != "hunter2" {
		t.Fatalf("Open = %q, %v", plain, err)
	}
}

func TestAnotherKeyCannotOpenIt(t *testing.T) {
	a, _ := LoadKey(t.TempDir(), "a.key")
	b, _ := LoadKey(t.TempDir(), "b.key")

	stored, _ := Seal(a, "secret")

	if _, err := Open(b, stored); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
	if _, err := Open(nil, stored); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("no key: err = %v, want ErrUnreadable", err)
	}
}

func TestAValueFromBeforeTheKeyIsReadAsIs(t *testing.T) {
	key, _ := LoadKey(t.TempDir(), "k")

	if plain, err := Open(key, "plain"); err != nil || plain != "plain" {
		t.Fatalf("Open = %q, %v", plain, err)
	}
}

func TestLoadKeyIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()

	a, err := LoadKey(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := LoadKey(dir, "k")
	if string(a) != string(b) {
		t.Fatal("a second load returned another key")
	}

	info, err := os.Stat(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 && os.PathSeparator == '/' {
		t.Errorf("key mode = %v, want 0600", mode)
	}
}
