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
// dynamic section at all; exec makes an ET_EXEC executable (not position
// independent, so not ET_DYN).
type fixture struct {
	needed         []string
	rpath, runpath string
	static, exec   bool
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
	typ := elf.ET_DYN
	if f.exec {
		typ = elf.ET_EXEC
	}
	h := elf.Header64{Type: uint16(typ), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
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

// A library the program's $ORIGIN path finds outside its build is not
// granted, and is reported, so a server that will not load says why. A
// search path that is not the build's ($LIB, a relative one) is not used.
func TestALibraryOutsideTheBuildIsNotGranted(t *testing.T) {
	dir := realTempDir(t)
	prog := filepath.Join(dir, "app", "server")
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

// A plugin (a ggml backend, opened at run time) in the build is granted,
// and its own libraries are walked from its own $ORIGIN; one already
// granted is named once.
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
	if !sameSet(got.files, []string{plugin, filepath.Join(dir, "libbase.so"), filepath.Join(dir, "libgpu-runtime.so")}) {
		t.Fatalf("granted %v", got.files)
	}
}

// An install tree: the program in bin, its libraries in ../lib. The
// parent of bin is the build, so the exact libraries its search path
// reaches there are granted, and nothing else in it: not a directory, not
// a library nothing names, not a file that is not a shared object.
func TestABinDirectorysParentIsTheBuild(t *testing.T) {
	dir := realTempDir(t)
	prog := filepath.Join(dir, "bin", "server")
	lib := filepath.Join(dir, "lib")
	writeELF(t, prog, fixture{needed: []string{"libX.so", "libZ.so", "libc.so.6"}, runpath: "$ORIGIN/../lib"})
	writeELF(t, filepath.Join(lib, "libX.so"), fixture{needed: []string{"libY.so"}, runpath: "$ORIGIN"})
	writeELF(t, filepath.Join(lib, "libY.so"), fixture{})
	mustWrite(t, filepath.Join(lib, "libZ.so"), "INPUT(-lz)") // a linker script, not a library
	writeELF(t, filepath.Join(lib, "libunused.so"), fixture{})
	if err := os.MkdirAll(filepath.Join(dir, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "share", "secret.txt"), "x")
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(got.files, []string{filepath.Join(lib, "libX.so"), filepath.Join(lib, "libY.so")}) {
		t.Fatalf("granted %v", got.files)
	}
	if !slices.Equal(got.origin, []string{lib}) || len(got.outside) != 0 {
		t.Fatalf("origin %v, outside %v", got.origin, got.outside)
	}
	if !slices.Equal(got.skipped, []string{filepath.Join(lib, "libZ.so")}) {
		t.Fatalf("skipped %v", got.skipped)
	}
	if runtime.GOOS != "linux" {
		return
	}
	p := ForProgram(prog, nil, nil)
	for _, d := range []string{dir, lib, filepath.Join(dir, "bin"), filepath.Join(dir, "share")} {
		if slices.Contains(p.Read, d) {
			t.Fatalf("the directory %s was granted:\n%s", d, strings.Join(p.Read, "\n"))
		}
	}
	if !slices.Contains(p.Read, filepath.Join(lib, "libX.so")) || !slices.Equal(p.LibraryPath, []string{lib}) {
		t.Fatalf("read %v, library path %v", p.Read, p.LibraryPath)
	}
}

// The parent of bin is the build only where it is not a filesystem root,
// /usr or /usr/local, which every policy reads anyway.
func TestBuildRoot(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/llama/bin/server":  "/opt/llama",
		"/home/u/llama/server":   "/home/u/llama",
		"/home/u/bin/server":     "/home/u",
		"/bin/server":            "/bin",
		"/usr/bin/server":        "/usr/bin",
		"/usr/local/bin/server":  "/usr/local/bin",
		"/srv/bin/x/bin/server2": "/srv/bin/x",
	} {
		if got := filepath.ToSlash(buildRoot(filepath.FromSlash(in))); got != want {
			t.Errorf("buildRoot(%s) = %s, want %s", in, got, want)
		}
	}
}

// A static program names no library and is no error, and a file that is
// not ELF is, so the caller falls back to its directory. A library or a
// plugin whose header is not ET_DYN (not ELF, or an ET_EXEC executable) is
// skipped and named, never granted.
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
	writeELF(t, prog, fixture{needed: []string{"libbad.so", "libexec.so"}, runpath: "$ORIGIN"})
	mustWrite(t, filepath.Join(dir, "libbad.so"), "not elf")
	writeELF(t, filepath.Join(dir, "libexec.so"), fixture{exec: true})
	plugin := filepath.Join(dir, "libggml-bad.so")
	mustWrite(t, plugin, "not elf")
	got, err := ownLibraries(prog, []string{plugin})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{plugin, filepath.Join(dir, "libbad.so"), filepath.Join(dir, "libexec.so")}
	if len(got.files) != 0 || !sameSet(got.skipped, want) {
		t.Fatalf("granted %v, skipped %v", got.files, got.skipped)
	}
}

// symlinkOrSkip makes link point at target, or skips where symlinks cannot
// be made.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
}

// A library in the build that is a symlink to an ELF file outside it is
// refused, by its real path, and reported: the path the loader opens being
// in the build is not enough.
func TestALibrarySymlinkOutOfTheBuildIsRefused(t *testing.T) {
	top := realTempDir(t)
	build, other := filepath.Join(top, "build"), filepath.Join(top, "other")
	writeELF(t, filepath.Join(other, "libprivate.so"), fixture{})
	prog := filepath.Join(build, "server")
	writeELF(t, prog, fixture{needed: []string{"libA.so"}, runpath: "$ORIGIN"})
	symlinkOrSkip(t, filepath.Join(other, "libprivate.so"), filepath.Join(build, "libA.so"))
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.files) != 0 || !slices.Equal(got.outside, []string{filepath.Join(build, "libA.so")}) {
		t.Fatalf("granted %v, outside %v", got.files, got.outside)
	}
}

// A library the search path finds outside the build is refused even when
// the file behind it is in the build: the path the loader opens must be in
// the build too.
func TestALibraryReachedFromOutsideTheBuildIsRefused(t *testing.T) {
	top := realTempDir(t)
	build := filepath.Join(top, "build")
	writeELF(t, filepath.Join(build, "libA-real.so"), fixture{})
	prog := filepath.Join(build, "server")
	writeELF(t, prog, fixture{needed: []string{"libA.so"}, runpath: "$ORIGIN/../ext"})
	symlinkOrSkip(t, filepath.Join(build, "libA-real.so"), filepath.Join(top, "ext", "libA.so"))
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.files) != 0 || !slices.Equal(got.outside, []string{filepath.Join(top, "ext", "libA.so")}) {
		t.Fatalf("granted %v, outside %v", got.files, got.outside)
	}
}

// A plugin is held to the same rule as a library: one in the build that is
// a symlink out of it, and one outside the build, are both refused and
// reported, and nothing they link is walked.
func TestAPluginOutsideTheBuildIsRefused(t *testing.T) {
	top := realTempDir(t)
	build, other := filepath.Join(top, "build"), filepath.Join(top, "other")
	writeELF(t, filepath.Join(other, "libprivate.so"), fixture{needed: []string{"libdep.so"}, runpath: "$ORIGIN"})
	writeELF(t, filepath.Join(other, "libdep.so"), fixture{})
	prog := filepath.Join(build, "server")
	writeELF(t, prog, fixture{})
	link := filepath.Join(build, "libggml-evil.so")
	symlinkOrSkip(t, filepath.Join(other, "libprivate.so"), link)
	away := filepath.Join(other, "libdep.so")
	got, err := ownLibraries(prog, []string{link, away})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.files) != 0 || !slices.Equal(got.outside, []string{link, away}) {
		t.Fatalf("granted %v, outside %v", got.files, got.outside)
	}
	if runtime.GOOS == "linux" {
		p := ForProgram(prog, nil, nil, link, away)
		for _, f := range []string{link, away, filepath.Join(other, "libprivate.so")} {
			if slices.Contains(p.Read, f) {
				t.Fatalf("%s granted:\n%s", f, strings.Join(p.Read, "\n"))
			}
		}
	}
}

// An absolute DT_NEEDED is looked for there alone and granted only inside
// the build ("build/../x" is outside); a relative one with a slash would be
// the working directory's, and is not looked for.
func TestAnAbsoluteNeededIsGrantedOnlyInsideTheBuild(t *testing.T) {
	top := realTempDir(t)
	build := filepath.Join(top, "build")
	writeELF(t, filepath.Join(top, "outside.so"), fixture{})
	writeELF(t, filepath.Join(build, "inside.so"), fixture{})
	prog := filepath.Join(build, "server")
	slash := filepath.ToSlash // a DT_NEEDED path is the ELF's, with "/" on any system
	writeELF(t, prog, fixture{needed: []string{slash(filepath.Join(top, "outside.so")), slash(build) + "/../outside.so",
		slash(filepath.Join(build, "inside.so")), "../outside.so", "sub/../../outside.so"}})
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.files, []string{filepath.Join(build, "inside.so")}) {
		t.Fatalf("granted %v", got.files)
	}
}

// $ORIGIN is a substitution only as glibc reads it: braced anywhere,
// unbraced only before a "/" or the end.
func TestOriginExpandsAsTheLoaderDoes(t *testing.T) {
	for in, want := range map[string]string{
		"$ORIGIN":           "/b",
		"$ORIGIN/lib":       "/b/lib",
		"${ORIGIN}x/lib":    "/bx/lib",
		"$ORIGINx/lib":      "$ORIGINx/lib",
		"/a:$ORIGIN":        "/a:/b",
		"$ORIGIN/$ORIGIN/x": "/b//b/x",
	} {
		if got, _ := expandOrigin(in, "/b"); got != want {
			t.Errorf("expandOrigin(%q) = %q, want %q", in, got, want)
		}
	}
	dir := realTempDir(t)
	prog := filepath.Join(dir, "server")
	writeELF(t, prog, fixture{needed: []string{"ox.so"}, runpath: "$ORIGINx"})
	writeELF(t, filepath.Join(dir+"x", "ox.so"), fixture{})
	defer os.RemoveAll(dir + "x")
	got, err := ownLibraries(prog, nil)
	if err != nil || len(got.files) != 0 || len(got.origin) != 0 {
		t.Fatalf("$ORIGINx expanded: %+v %v", got, err)
	}
}

// A panic in the ELF parser is an ELF it cannot read, not a crash.
func TestAPanicReadingAnELFIsAnError(t *testing.T) {
	old := openELF
	openELF = func(string) (*elf.File, error) { panic("crafted input") }
	defer func() { openELF = old }()
	prog := filepath.Join(realTempDir(t), "server")
	mustWrite(t, prog, "x")
	if _, err := ownLibrariesSafe(prog, nil); err == nil || !strings.Contains(err.Error(), "crafted input") {
		t.Fatalf("err %v", err)
	}
}

// withInstallRoots sets the install roots for one test.
func withInstallRoots(t *testing.T, dirs ...string) {
	t.Helper()
	old := InstallRoots()
	SetInstallRoots(dirs...)
	t.Cleanup(func() { SetInstallRoots(old...) })
}

// An install root that is a symlink counts by its real path, so a program
// beneath it may fall back. A directory symlink planted inside an install
// root (a compromised worker's venv) is judged by where it really points.
func TestASymlinkedInstallRootAndAPlantedLink(t *testing.T) {
	top := realTempDir(t)
	alt := filepath.Join(top, "altstore")
	if err := os.MkdirAll(filepath.Join(alt, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, alt, filepath.Join(top, "storelink"))
	withInstallRoots(t, filepath.Join(top, "storelink"))
	if ok, why := directoryFallback(filepath.Join(top, "storelink", "b")); !ok {
		t.Fatalf("refused beneath a symlinked root: %s", why)
	}
	if ok, why := directoryFallback(filepath.Join(alt, "b")); !ok {
		t.Fatalf("refused by its real path: %s", why)
	}
	secrets := filepath.Join(top, "secrets")
	if err := os.MkdirAll(secrets, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, secrets, filepath.Join(alt, "evil"))
	if ok, _ := directoryFallback(filepath.Join(top, "storelink", "evil")); ok {
		t.Fatal("a link planted in the store reached the directory it points at")
	}
}

// The loader loads a name once, by the name as written: an absolute
// DT_NEEDED and the same base name by plain name are two names (a library
// with no SONAME matches only its path), and each is looked up on its own.
func TestANeededNameIsLookedUpOnceAsWritten(t *testing.T) {
	dir := realTempDir(t)
	sub := filepath.Join(dir, "sub", "libA.so")
	writeELF(t, sub, fixture{})
	writeELF(t, filepath.Join(dir, "libA.so"), fixture{})
	prog := filepath.Join(dir, "server")
	writeELF(t, prog, fixture{needed: []string{filepath.ToSlash(sub), "libA.so", filepath.ToSlash(sub)}, runpath: "$ORIGIN"})
	got, err := ownLibraries(prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.files, []string{sub, filepath.Join(dir, "libA.so")}) {
		t.Fatalf("granted %v", got.files)
	}
}

// The directory fallback is allowed only strictly beneath an install root,
// and never for a filesystem root, the home directory or the temporary
// directory, whatever the roots say.
func TestTheDirectoryFallbackIsOnlyInsideTheInstallStore(t *testing.T) {
	store := realTempDir(t)
	withInstallRoots(t, filepath.Join(store, "bin"), "")
	if ok, why := directoryFallback(filepath.Join(store, "bin", "llama")); !ok {
		t.Fatalf("refused inside the store: %s", why)
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(store, "bin"), store, filepath.Join(store, "logs"), realTempDir(t),
		filepath.VolumeName(store) + string(filepath.Separator), home, os.TempDir()} {
		if dir == "" {
			continue
		}
		if ok, _ := directoryFallback(dir); ok {
			t.Errorf("%s allowed", dir)
		}
	}
	// Even an install root at the home or temporary directory does not
	// make those directories a fallback.
	withInstallRoots(t, filepath.Dir(home), filepath.Dir(realPath(os.TempDir())))
	for _, dir := range []string{home, os.TempDir()} {
		if ok, why := directoryFallback(dir); ok || why == "not inside Brainiac's install store" {
			t.Errorf("%s: %v %q", dir, ok, why)
		}
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
