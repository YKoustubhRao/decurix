package isolate

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fixture is a tiny ELF file: a dynamic section naming needed libraries and
// a search path, which is all the walk reads. static makes one with no
// dynamic section at all.
type fixture struct {
	needed         []string
	rpath, runpath string
	static         bool
}

// writeELF writes f as a 64-bit little-endian ELF file at path.
func writeELF(t *testing.T, path string, f fixture) {
	t.Helper()
	dynstr := []byte{0}
	str := func(s string) uint64 {
		off := uint64(len(dynstr))
		dynstr = append(append(dynstr, s...), 0)
		return off
	}
	var dyn []elf.Dyn64
	for _, n := range f.needed {
		dyn = append(dyn, elf.Dyn64{Tag: int64(elf.DT_NEEDED), Val: str(n)})
	}
	if f.rpath != "" {
		dyn = append(dyn, elf.Dyn64{Tag: int64(elf.DT_RPATH), Val: str(f.rpath)})
	}
	if f.runpath != "" {
		dyn = append(dyn, elf.Dyn64{Tag: int64(elf.DT_RUNPATH), Val: str(f.runpath)})
	}
	dyn = append(dyn, elf.Dyn64{Tag: int64(elf.DT_NULL)})
	shstr := []byte("\x00.dynstr\x00.dynamic\x00.shstrtab\x00")
	var dynBytes bytes.Buffer
	binary.Write(&dynBytes, binary.LittleEndian, dyn)

	const ehsize, shentsize = 64, 64
	align := func(n int) int { return (n + 7) &^ 7 }
	dynstrOff := ehsize
	dynOff := align(dynstrOff + len(dynstr))
	shstrOff := align(dynOff + dynBytes.Len())
	shOff := align(shstrOff + len(shstr))
	sections := []elf.Section64{{}, // the null section
		{Name: 1, Type: uint32(elf.SHT_STRTAB), Off: uint64(dynstrOff), Size: uint64(len(dynstr)), Addralign: 1},
		{Name: 9, Type: uint32(elf.SHT_DYNAMIC), Off: uint64(dynOff), Size: uint64(dynBytes.Len()), Link: 1, Addralign: 8, Entsize: 16},
		{Name: 18, Type: uint32(elf.SHT_STRTAB), Off: uint64(shstrOff), Size: uint64(len(shstr)), Addralign: 1},
	}
	shstrndx := 3
	if f.static {
		sections = []elf.Section64{{}, sections[3]}
		sections[1].Name = 18
		shstrndx = 1
	}
	h := elf.Header64{Type: uint16(elf.ET_DYN), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
		Shoff: uint64(shOff), Ehsize: ehsize, Shentsize: shentsize, Shnum: uint16(len(sections)), Shstrndx: uint16(shstrndx)}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS], h.Ident[elf.EI_DATA], h.Ident[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)

	out := make([]byte, shOff)
	var hb bytes.Buffer
	binary.Write(&hb, binary.LittleEndian, h)
	copy(out, hb.Bytes())
	copy(out[dynstrOff:], dynstr)
	copy(out[dynOff:], dynBytes.Bytes())
	copy(out[shstrOff:], shstr)
	var sb bytes.Buffer
	binary.Write(&sb, binary.LittleEndian, sections)
	out = append(out, sb.Bytes()...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o755); err != nil {
		t.Fatal(err)
	}
}

// The fixture reads back as what it was written as, so the tests below test
// the walk and not the fixture.
func TestTheELFFixtureReadsBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prog")
	writeELF(t, p, fixture{needed: []string{"libA.so", "libc.so.6"}, runpath: "$ORIGIN:/opt/x"})
	o, err := readELF(p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.needed, []string{"libA.so", "libc.so.6"}) || !slices.Equal(o.runpath, []string{"$ORIGIN", "/opt/x"}) || o.rpath != nil {
		t.Fatalf("%+v", o)
	}
}

// A release build: the program needs a library beside it through $ORIGIN,
// which needs another in a subdirectory through ${ORIGIN}/deps. Both are
// granted, and the system's library is not looked for in the build; a
// library nothing names, and any other file, are not granted.
func TestOwnLibrariesAreWhatTheELFNamesInsideTheBuild(t *testing.T) {
	dir := realTempDir(t)
	prog := filepath.Join(dir, "server")
	writeELF(t, prog, fixture{needed: []string{"libA.so", "libc.so.6"}, runpath: "$ORIGIN"})
	writeELF(t, filepath.Join(dir, "libA.so"), fixture{needed: []string{"libB.so", "libm.so.6"}, runpath: "${ORIGIN}/deps"})
	writeELF(t, filepath.Join(dir, "deps", "libB.so"), fixture{needed: []string{"libc.so.6"}})
	writeELF(t, filepath.Join(dir, "libunused.so"), fixture{})
	mustWrite(t, filepath.Join(dir, "secret.txt"), "x")
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "libA.so"), filepath.Join(dir, "deps", "libB.so")}
	if !sameSet(got.files, want) {
		t.Fatalf("granted %v, want %v", got.files, want)
	}
	if !slices.Equal(got.origin, []string{dir}) {
		t.Fatalf("origin path %v, want %v", got.origin, dir)
	}
	if len(got.outside) != 0 {
		t.Fatalf("outside %v", got.outside)
	}
}

// DT_RPATH, unlike DT_RUNPATH, is searched for the libraries of what the
// program loads too: a library with no search path of its own finds its
// dependency through the program's.
func TestTheProgramsRPATHServesItsLibrariesToo(t *testing.T) {
	dir := realTempDir(t)
	prog := filepath.Join(dir, "bin", "server")
	writeELF(t, prog, fixture{needed: []string{"libA.so"}, rpath: "$ORIGIN/lib"})
	writeELF(t, filepath.Join(dir, "bin", "lib", "libA.so"), fixture{needed: []string{"libB.so"}})
	writeELF(t, filepath.Join(dir, "bin", "lib", "libB.so"), fixture{})
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "bin", "lib")
	if !sameSet(got.files, []string{filepath.Join(lib, "libA.so"), filepath.Join(lib, "libB.so")}) {
		t.Fatalf("granted %v", got.files)
	}
	if !slices.Equal(got.origin, []string{lib}) {
		t.Fatalf("origin path %v", got.origin)
	}
}

// A library the program's $ORIGIN path finds outside its own directory is
// not granted, and is reported, so a server that will not load says why.
// A search path that is not the build's ($LIB, a relative one) is not used.
func TestALibraryOutsideTheBuildIsNotGranted(t *testing.T) {
	dir := realTempDir(t)
	prog := filepath.Join(dir, "bin", "server")
	writeELF(t, prog, fixture{needed: []string{"libX.so", "libY.so"}, runpath: "$ORIGIN/../lib:$LIB/z:rel"})
	writeELF(t, filepath.Join(dir, "lib", "libX.so"), fixture{})
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.files) != 0 || len(got.origin) != 0 {
		t.Fatalf("granted %v, origin %v", got.files, got.origin)
	}
	if !slices.Equal(got.outside, []string{filepath.Join(dir, "lib", "libX.so")}) {
		t.Fatalf("outside %v", got.outside)
	}
}

// A plugin (a ggml backend, opened at run time) has its own libraries
// walked from its own $ORIGIN; one already granted is named once.
func TestAPluginsLibrariesAreGranted(t *testing.T) {
	dir := realTempDir(t)
	prog := filepath.Join(dir, "server")
	writeELF(t, prog, fixture{needed: []string{"libbase.so"}, runpath: "$ORIGIN"})
	writeELF(t, filepath.Join(dir, "libbase.so"), fixture{})
	plugin := filepath.Join(dir, "libplug-gpu.so")
	writeELF(t, plugin, fixture{needed: []string{"libbase.so", "libgpu-runtime.so"}, runpath: "$ORIGIN"})
	writeELF(t, filepath.Join(dir, "libgpu-runtime.so"), fixture{})
	got, err := ownLibraries(prog, []string{plugin})
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(got.files, []string{filepath.Join(dir, "libbase.so"), filepath.Join(dir, "libgpu-runtime.so")}) {
		t.Fatalf("granted %v", got.files)
	}
}

// A static program names no library and is no error; a file that is not
// ELF, or a library in the build that is not, is an error, so the caller
// falls back rather than grant too little.
func TestStaticAndUnreadablePrograms(t *testing.T) {
	dir := realTempDir(t)
	static := filepath.Join(dir, "static")
	writeELF(t, static, fixture{static: true})
	if got, err := ownLibraries(static, nil); err != nil || len(got.files) != 0 {
		t.Fatalf("static: %v %v", got, err)
	}
	script := filepath.Join(dir, "script")
	mustWrite(t, script, "#!/bin/sh\n")
	if _, err := ownLibraries(script, nil); err == nil {
		t.Fatal("a script read as ELF")
	}
	prog := filepath.Join(dir, "server")
	writeELF(t, prog, fixture{needed: []string{"libbad.so"}, runpath: "$ORIGIN"})
	mustWrite(t, filepath.Join(dir, "libbad.so"), "not elf")
	if _, err := ownLibraries(prog, nil); err == nil || !strings.Contains(err.Error(), "libbad.so") {
		t.Fatalf("a broken library in the build: %v", err)
	}
}

// A symlinked soname (libA.so.0 -> libA.so.0.1, as a release ships them) is
// granted by both paths: the view keeps the link and binds the file.
func TestASonameSymlinkIsGrantedWithItsFile(t *testing.T) {
	dir := realTempDir(t)
	writeELF(t, filepath.Join(dir, "libA.so.0.1"), fixture{})
	if err := os.Symlink("libA.so.0.1", filepath.Join(dir, "libA.so.0")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	prog := filepath.Join(dir, "server")
	writeELF(t, prog, fixture{needed: []string{"libA.so.0"}, runpath: "$ORIGIN"})
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Join(dir, "libA.so.0.1"))
	if !slices.Contains(got.files, filepath.Join(dir, "libA.so.0")) || !slices.Contains(got.files, real) {
		t.Fatalf("granted %v", got.files)
	}
}

// The origin path goes after what LD_LIBRARY_PATH already holds, as the
// loader searches LD_LIBRARY_PATH before DT_RUNPATH.
func TestWithLibraryPath(t *testing.T) {
	if got := withLibraryPath([]string{"PATH=/bin"}, nil); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Fatal(got)
	}
	if got := withLibraryPath([]string{"PATH=/bin"}, []string{"/b", "/c"}); !slices.Equal(got, []string{"PATH=/bin", "LD_LIBRARY_PATH=/b:/c"}) {
		t.Fatal(got)
	}
	if got := withLibraryPath([]string{"LD_LIBRARY_PATH=/a", "X=1"}, []string{"/b"}); !slices.Equal(got, []string{"LD_LIBRARY_PATH=/a:/b", "X=1"}) {
		t.Fatal(got)
	}
	if got := withLibraryPath([]string{"LD_LIBRARY_PATH="}, []string{"/b"}); !slices.Equal(got, []string{"LD_LIBRARY_PATH=/b"}) {
		t.Fatal(got)
	}
}

// ForProgram grants the program and the libraries its ELF loads from its
// build, not every other file in its directory, nor the directory.
func TestForProgramGrantsTheProgramNotItsWholeDirectory(t *testing.T) {
	dir := realTempDir(t)
	prog := filepath.Join(dir, "server")
	writeELF(t, prog, fixture{needed: []string{"libggml.so"}, runpath: "$ORIGIN"})
	writeELF(t, filepath.Join(dir, "libggml.so"), fixture{})
	writeELF(t, filepath.Join(dir, "libother-tool.so"), fixture{})
	mustWrite(t, filepath.Join(dir, "secret.txt"), "x")
	p := ForProgram(prog, nil, nil)
	if slices.Contains(p.Read, dir) {
		t.Fatalf("the program's directory was granted:\n%s", strings.Join(p.Read, "\n"))
	}
	for _, n := range []string{"secret.txt", "libother-tool.so"} {
		if slices.Contains(p.Read, filepath.Join(dir, n)) {
			t.Fatalf("%s was granted:\n%s", n, strings.Join(p.Read, "\n"))
		}
	}
	if !slices.Contains(p.Read, prog) {
		t.Fatalf("the program was not granted:\n%s", strings.Join(p.Read, "\n"))
	}
	if runtime.GOOS == "linux" {
		if !slices.Contains(p.Read, filepath.Join(dir, "libggml.so")) {
			t.Fatalf("its library was not granted:\n%s", strings.Join(p.Read, "\n"))
		}
		if !slices.Equal(p.LibraryPath, []string{dir}) {
			t.Fatalf("library path %v", p.LibraryPath)
		}
	}
}

// realTempDir is a test directory by its real path (Windows names its temp
// directory by a short name, which EvalSymlinks lengthens).
func realTempDir(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
