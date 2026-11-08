package isolate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// managedPython lays out a managed interpreter's prefix: bin/<name> and
// lib/python3.12 with the os.py landmark (when landmark), and a sibling
// python3.11 tree that only an unversioned name may claim.
func managedPython(t *testing.T, name string, landmark bool) (prefix, program string) {
	t.Helper()
	prefix = filepath.Join(t.TempDir(), "cpython-3.12.9-linux-x86_64-gnu")
	program = filepath.Join(prefix, "bin", name)
	for _, d := range []string{filepath.Dir(program), filepath.Join(prefix, "lib", "python3.12", "lib-dynload"), filepath.Join(prefix, "lib", "python3.11")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(program, []byte("not an ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if landmark {
		if err := os.WriteFile(filepath.Join(prefix, "lib", "python3.12", "os.py"), []byte("# os\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return prefix, program
}

func realOf(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// FIX-BR-1: a managed Python's standard library (lib/python3.N beside its
// bin, found by the os.py landmark) is granted read-only, and only that
// directory: not the prefix, not a version without the landmark.
func TestAManagedPythonGetsItsStandardLibrary(t *testing.T) {
	prefix, program := managedPython(t, "python3.12", true)
	got := pythonStdlib(program)
	want := realOf(t, filepath.Join(prefix, "lib", "python3.12"))
	if len(got) != 1 || got[0] != want {
		t.Fatalf("granted %v, want [%s]", got, want)
	}
	pol := ForProgram(program, nil, nil)
	if !slices.Contains(pol.Read, want) || slices.Contains(pol.Read, prefix) || slices.Contains(pol.Read, realOf(t, prefix)) {
		t.Fatalf("policy reads %v", pol.Read)
	}
	if slices.Contains(pol.Write, want) {
		t.Fatal("the standard library is writable")
	}
}

// A tree without the landmark is not a Python's library, and a program
// that is not named python gets nothing from its lib directory.
func TestOnlyALandmarkedTreeOfAPythonIsGranted(t *testing.T) {
	_, program := managedPython(t, "python3.12", false)
	if got := pythonStdlib(program); len(got) != 0 {
		t.Fatalf("a tree without os.py was granted: %v", got)
	}
	prefix, _ := managedPython(t, "python3.12", true)
	other := filepath.Join(prefix, "bin", "llama-server")
	if err := os.WriteFile(other, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := pythonStdlib(other); len(got) != 0 {
		t.Fatalf("a program that is not Python was granted %v", got)
	}
	// A program not in a bin directory has no prefix to look in.
	loose := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(loose, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := pythonStdlib(loose); len(got) != 0 {
		t.Fatalf("a loose program was granted %v", got)
	}
}

// An unversioned name claims every landmarked version; a versioned one
// only its own.
func TestAnUnversionedPythonGetsEveryLandmarkedVersion(t *testing.T) {
	prefix, program := managedPython(t, "python3", true)
	if err := os.WriteFile(filepath.Join(prefix, "lib", "python3.11", "os.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pythonStdlib(program); len(got) != 2 {
		t.Fatalf("python3 got %v", got)
	}
	versioned := filepath.Join(prefix, "bin", "python3.12")
	if err := os.WriteFile(versioned, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := pythonStdlib(versioned); len(got) != 1 || filepath.Base(got[0]) != "python3.12" {
		t.Fatalf("python3.12 got %v", got)
	}
}

// A system interpreter's library is already readable: nothing is added.
func TestASystemPythonAddsNothing(t *testing.T) {
	for _, p := range []string{"/usr/bin/python3", "/usr/local/bin/python3.12"} {
		if got := pythonStdlib(p); len(got) != 0 {
			t.Fatalf("%s: %v", p, got)
		}
	}
}

// A venv's python is a link to its base interpreter: the base's library
// is granted, by its real path.
func TestAVenvPythonGrantsItsBasesLibrary(t *testing.T) {
	prefix, program := managedPython(t, "python3.12", true)
	venv := filepath.Join(t.TempDir(), "venv", "bin")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(venv, "python")
	if err := os.Symlink(program, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	got := pythonStdlib(link)
	if len(got) != 1 || got[0] != realOf(t, filepath.Join(prefix, "lib", "python3.12")) {
		t.Fatalf("venv python got %v", got)
	}
}
