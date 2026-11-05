//go:build linux

package isolate

import (
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// captureLog collects what the package logs during one test.
func captureLog(t *testing.T) *[]string {
	t.Helper()
	var logged []string
	old := logf
	logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	t.Cleanup(func() { logf = old })
	return &logged
}

// A program in Brainiac's install store whose ELF cannot be read gets its
// directory, read-only, and that is said once; nothing is granted writable.
func TestAnUnreadableProgramFallsBackToItsDirectoryReadOnly(t *testing.T) {
	store := realTempDir(t)
	withInstallRoots(t, store)
	dir := filepath.Join(store, "build")
	prog := filepath.Join(dir, "server")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, prog, "#!/bin/sh\n")
	logged := captureLog(t)
	p := ForProgram(prog, nil, nil)
	if !slices.Contains(p.Read, dir) {
		t.Fatalf("the directory was not granted:\n%s", strings.Join(p.Read, "\n"))
	}
	if slices.Contains(p.Write, dir) || len(p.LibraryPath) != 0 {
		t.Fatalf("write %v, library path %v", p.Write, p.LibraryPath)
	}
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "granting its directory") {
		t.Fatalf("logged %q", *logged)
	}
}

// Outside the install store the fallback is refused: the program alone is
// granted, and the log says why it may not start. A panic while reading
// the ELF is treated the same way.
func TestAnUnreadableProgramOutsideTheStoreGetsOnlyItself(t *testing.T) {
	withInstallRoots(t, filepath.Join(realTempDir(t), "store"))
	dir := realTempDir(t)
	prog := filepath.Join(dir, "server")
	mustWrite(t, prog, "#!/bin/sh\n")
	logged := captureLog(t)
	p := ForProgram(prog, nil, nil)
	if slices.Contains(p.Read, dir) || !slices.Contains(p.Read, prog) || len(p.LibraryPath) != 0 {
		t.Fatalf("read %v, library path %v", p.Read, p.LibraryPath)
	}
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "not inside Brainiac's install store") ||
		!strings.Contains((*logged)[0], "granting the program alone") {
		t.Fatalf("logged %q", *logged)
	}
	old := openELF
	openELF = func(string) (*elf.File, error) { panic("crafted input") }
	defer func() { openELF = old }()
	elfProg := filepath.Join(dir, "elfserver")
	writeELF(t, elfProg, fixture{})
	if p := ForProgram(elfProg, nil, nil); slices.Contains(p.Read, dir) || !slices.Contains(p.Read, elfProg) {
		t.Fatalf("after a panic: read %v", p.Read)
	}
}

// A real program of this system: its libraries are the system's, so none
// is granted from its build, and reading it is no error.
func TestASystemProgramNeedsNothingFromItsDirectory(t *testing.T) {
	for _, prog := range []string{"/bin/sh", "/usr/bin/env"} {
		if _, err := os.Stat(prog); err != nil {
			continue
		}
		got, err := ownLibraries(prog, nil)
		if err != nil {
			t.Fatalf("%s: %v", prog, err)
		}
		if len(got.files) != 0 || len(got.outside) != 0 {
			t.Fatalf("%s: %+v", prog, got)
		}
	}
}

// In a mount view a listed directory holds only what is granted, and the
// program's origin path reaches it as LD_LIBRARY_PATH after the one it had.
// With Landlock alone the names are listed, never read, and the variable
// is left as it was (the loader can read /proc/self/exe there).
func TestAListedDirectoryAndTheOriginPathInAView(t *testing.T) {
	if os.Getenv("BRAINIAC_LIBVIEW_CHILD") == "1" {
		libViewChild()
	}
	for _, view := range []bool{true, false} {
		t.Run(fmt.Sprintf("view=%v", view), func(t *testing.T) {
			if ok, err := CanConfine(); view && !ok {
				t.Skipf("this system refuses the namespaces: %v", err)
			}
			if !view && !CanSandbox() {
				t.Skip("no Landlock on this system")
			}
			dir := realTempDir(t)
			mustWrite(t, filepath.Join(dir, "libggml-cpu-x64.so"), "granted")
			mustWrite(t, filepath.Join(dir, "secret.txt"), "secret")
			self, _ := os.Executable()
			want := "/first"
			if view {
				want = "/first:/brainiac-origin"
			}
			cmd := exec.Command(self, "-test.run=TestAListedDirectoryAndTheOriginPathInAView")
			cmd.Env = Env("BRAINIAC_LIBVIEW_CHILD=1", "LD_LIBRARY_PATH=/first", "CHILD_DIR="+dir,
				"CHILD_WANT_LDPATH="+want, fmt.Sprintf("CHILD_VIEW=%v", view))
			if view {
				NoNetwork(cmd)
			}
			p := ForProgram(self, []string{filepath.Join(dir, "libggml-cpu-x64.so")}, nil)
			p.List = []string{dir}
			p.LibraryPath = []string{"/brainiac-origin"}
			if err := Wrap(cmd, p); err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}

func libViewChild() {
	fail := func(code int, what string, err error) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
		os.Exit(code)
	}
	dir := os.Getenv("CHILD_DIR")
	if got := os.Getenv("LD_LIBRARY_PATH"); got != os.Getenv("CHILD_WANT_LDPATH") {
		fail(61, "LD_LIBRARY_PATH is "+got, nil)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fail(62, "could not list its build", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if os.Getenv("CHILD_VIEW") == "true" && !slices.Equal(names, []string{"libggml-cpu-x64.so"}) {
		fail(63, fmt.Sprintf("the view lists %v", names), nil)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "libggml-cpu-x64.so")); err != nil {
		fail(64, "could not read a granted library", err)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "secret.txt")); err == nil {
		fail(65, "read a file in a directory it may only list", nil)
	}
	os.Exit(0)
}
