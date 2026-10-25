package hub

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	mode             int64
	kind             byte // tar type flag; 0 = regular file
}

// makeTar writes entries as a .tar or .tar.gz into dir and returns its path.
func makeTar(t *testing.T, dir, name string, entries []entry) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.kind, Linkname: e.link}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.kind == 0 {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.kind == 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	data := buf.Bytes()
	if strings.HasSuffix(name, ".gz") || strings.HasSuffix(name, ".tgz") {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write(data)
		zw.Close()
		data = gz.Bytes()
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// makeZip writes entries as a .zip into dir and returns its path.
func makeZip(t *testing.T, dir, name string, entries []entry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := os.FileMode(e.mode)
		if mode == 0 {
			mode = 0o644
		}
		if strings.HasSuffix(e.name, "/") {
			mode |= os.ModeDir
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.body))
	}
	zw.Close()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func checkExecutable(t *testing.T, p string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("%s lost its executable bit (%v)", filepath.Base(p), fi.Mode())
	}
}

// symlinksWork reports whether this machine lets a test create symlinks.
func symlinksWork(t *testing.T) bool {
	d := t.TempDir()
	return os.Symlink("x", filepath.Join(d, "probe")) == nil
}

// noLeftovers fails when Unpack left a temporary directory in parent.
func noLeftovers(t *testing.T, parent string) {
	t.Helper()
	items, _ := os.ReadDir(parent)
	for _, it := range items {
		if strings.Contains(it.Name(), ".unpack-") || strings.Contains(it.Name(), ".old-") {
			t.Errorf("left behind: %s", it.Name())
		}
	}
}

func TestUnpackZip(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()
	z := makeZip(t, src, "runtime.zip", []entry{
		{name: "pkg/"},
		{name: "pkg/bin/tool", body: "#!/bin/sh\n", mode: 0o755},
		{name: "pkg/readme.txt", body: "hello"},
	})
	dest := filepath.Join(out, "llama")
	if err := Unpack(z, dest); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "pkg", "readme.txt")); got != "hello" {
		t.Errorf("readme = %q", got)
	}
	checkExecutable(t, filepath.Join(dest, "pkg", "bin", "tool"))
	noLeftovers(t, out)
}

func TestUnpackTarGzAndHardLink(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()
	a := makeTar(t, src, "model.tar.gz", []entry{
		{name: "model/", kind: tar.TypeDir, mode: 0o755},
		{name: "model/encoder.onnx", body: "weights"},
		{name: "model/run", body: "#!/bin/sh\n", mode: 0o4755}, // setuid must be dropped
		{name: "model/copy.onnx", kind: tar.TypeLink, link: "model/encoder.onnx"},
	})
	dest := filepath.Join(out, "asr")
	if err := Unpack(a, dest); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "model", "copy.onnx")); got != "weights" {
		t.Errorf("hard link copy = %q", got)
	}
	checkExecutable(t, filepath.Join(dest, "model", "run"))
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dest, "model", "run"))
		if fi.Mode()&os.ModeSetuid != 0 {
			t.Error("setuid survived unpacking")
		}
	}
	noLeftovers(t, out)
}

func TestUnpackTarBz2Fixture(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "fixture")
	if err := Unpack(filepath.Join("testdata", "sample.tar.bz2"), dest); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "model", "tokens.txt")); got != "a 0\nb 1\n" {
		t.Errorf("tokens.txt = %q", got)
	}
	checkExecutable(t, filepath.Join(dest, "model", "run.sh"))
}

func TestUnpackReplacesExistingDir(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()
	dest := filepath.Join(out, "rt")
	if err := Unpack(makeZip(t, src, "a.zip", []entry{{name: "old.txt", body: "1"}}), dest); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(makeZip(t, src, "b.zip", []entry{{name: "new.txt", body: "2"}}), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "old.txt")); err == nil {
		t.Error("the old unpack was not replaced")
	}
	if readFile(t, filepath.Join(dest, "new.txt")) != "2" {
		t.Error("the new unpack is missing")
	}
	noLeftovers(t, out)
}

func TestUnpackRefusesEscapes(t *testing.T) {
	cases := map[string]func(dir string) string{
		"zip slip": func(dir string) string {
			return makeZip(t, dir, "slip.zip", []entry{{name: "ok.txt", body: "fine"}, {name: "../evil.txt", body: "pwned"}})
		},
		"zip backslash slip": func(dir string) string {
			return makeZip(t, dir, "slip2.zip", []entry{{name: `..\evil.txt`, body: "pwned"}})
		},
		"tar absolute": func(dir string) string {
			return makeTar(t, dir, "abs.tar", []entry{{name: "/evil.txt", body: "pwned"}})
		},
		"tar nested climb": func(dir string) string {
			return makeTar(t, dir, "climb.tar", []entry{{name: "a/../../evil.txt", body: "pwned"}})
		},
		"symlink up": func(dir string) string {
			return makeTar(t, dir, "up.tar", []entry{{name: "lib/up", kind: tar.TypeSymlink, link: "../../etc"}})
		},
		"symlink absolute": func(dir string) string {
			return makeTar(t, dir, "absl.tar", []entry{{name: "lib/abs", kind: tar.TypeSymlink, link: "/etc/passwd"}})
		},
		"hard link outside": func(dir string) string {
			return makeTar(t, dir, "hl.tar", []entry{{name: "x", kind: tar.TypeLink, link: "../../etc/passwd"}})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			outer := t.TempDir()
			out := filepath.Join(outer, "inner")
			os.MkdirAll(out, 0o755)
			dest := filepath.Join(out, "dest")
			if err := Unpack(build(src), dest); err == nil {
				t.Fatal("unpacked an archive that escapes its folder")
			}
			if _, err := os.Stat(dest); err == nil {
				t.Error("a refused archive left its folder in place")
			}
			for _, p := range []string{filepath.Join(out, "evil.txt"), filepath.Join(outer, "evil.txt")} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s was written outside the folder", p)
				}
			}
			noLeftovers(t, out)
		})
	}
}

func TestUnpackSymlinks(t *testing.T) {
	if !symlinksWork(t) {
		t.Skip("this machine does not let tests create symlinks")
	}
	src := t.TempDir()
	dest := filepath.Join(t.TempDir(), "lib")
	ok := makeTar(t, src, "ok.tar.gz", []entry{
		{name: "lib/libggml.so.1", body: "elf"},
		{name: "lib/libggml.so", kind: tar.TypeSymlink, link: "libggml.so.1"},
	})
	if err := Unpack(ok, dest); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "lib", "libggml.so")); got != "elf" {
		t.Errorf("through the link: %q", got)
	}

	through := makeTar(t, src, "through.tar", []entry{
		{name: "pkg/sub/", kind: tar.TypeDir, mode: 0o755},
		{name: "pkg/link", kind: tar.TypeSymlink, link: "sub"},
		{name: "pkg/link/x.txt", body: "via a link"},
	})
	if err := Unpack(through, filepath.Join(t.TempDir(), "t")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a path through a symlink: %v", err)
	}
}

func TestUnpackUnsupportedFormats(t *testing.T) {
	for _, n := range []string{"x.tar.xz", "x.7z", "x.tar.zst", "x.rar"} {
		if err := Unpack(filepath.Join(t.TempDir(), n), filepath.Join(t.TempDir(), "d")); err == nil {
			t.Errorf("%s: no error", n)
		}
	}
	err := Unpack("model.tar.xz", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "standard library") {
		t.Errorf("xz error does not say why: %v", err)
	}
}
