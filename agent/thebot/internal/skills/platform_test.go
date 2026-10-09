package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// forShell is the command a test sends. Tests written while workers could
// also be PowerShell name a second form, which is no longer used.
func forShell(bash, _ string) string {
	return bash
}

// stubProgram puts a stand-in for a program first on PATH: a shell script.
func stubProgram(t *testing.T, name, shBody, _ string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+shBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubWorkerTools stands in for the worker's own gui and office commands,
// programs on the PATH, each a script (shGui, shOffice).
func stubWorkerTools(t *testing.T, shGui, shOffice, _ string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"gui": shGui, "office": shOffice} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// workerPython makes python3 mean a real Python in the commands a test runs,
// which on a Linux desktop it already does.
func workerPython(t *testing.T) {
	t.Helper()
}
