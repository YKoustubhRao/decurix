package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// The live bug this pins: an agent created a Python venv named "myenv" -
// an agent-chosen name, not any fixed convention - to install real
// dependencies. Nothing in the name-based ignoreDirs list caught it, so
// the venv's own compiled python3 binary (and its symlinks) got snapshot-
// tracked as ordinary files, then auto-ingested into the Data RAG as
// "text": a multi-megabyte ELF executable chunked into 6133 pieces, three
// times over. A directory is structurally a venv - regardless of its
// name - the instant it contains a pyvenv.cfg file, exactly how pip/venv
// themselves identify one.
func TestSnapshotSkipsVenvDirectoriesRegardlessOfName(t *testing.T) {
	root := t.TempDir()
	ws, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "app.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, venvName := range []string{"myenv", "dashenv2", "some_weird_venv_name"} {
		venvDir := filepath.Join(root, venvName, "bin")
		if err := os.MkdirAll(venvDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, venvName, "pyvenv.cfg"), []byte("home = /usr"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Simulate a compiled binary sitting inside it.
		if err := os.WriteFile(filepath.Join(venvDir, "python3"), []byte{0x7f, 'E', 'L', 'F', 0, 0, 0}, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := ws.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	if _, ok := snap["app.py"]; !ok {
		t.Error("Snapshot() dropped a real, ordinary file - the venv skip must not be overbroad")
	}
	for relPath := range snap {
		if relPath != "app.py" {
			t.Errorf("Snapshot() included %q - a venv-internal file (any directory containing pyvenv.cfg must be skipped entirely, whatever it's named)", relPath)
		}
	}
}

// An ordinary directory that merely happens to contain a file named
// something else entirely must not be treated as a venv.
func TestSnapshotDoesNotSkipOrdinaryDirectories(t *testing.T) {
	root := t.TempDir()
	ws, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := ws.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if _, ok := snap["src/main.go"]; !ok {
		t.Error("Snapshot() dropped a real file in an ordinary directory")
	}
}
