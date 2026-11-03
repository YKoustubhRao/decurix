package desktop

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/YKoustubhRao/decurix"
)

// contextNames tars fsys the way a build does and returns what the daemon
// would be sent: name -> content.
func contextNames(t *testing.T, fsys fs.FS) map[string]string {
	t.Helper()

	var buf bytes.Buffer
	if err := tarBuildContext(&buf, fsys); err != nil {
		t.Fatalf("tarBuildContext: %v", err)
	}

	files := map[string]string{}
	tr := tar.NewReader(&buf)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return files
		}

		if err != nil {
			t.Fatal(err)
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}

		files[header.Name] = string(data)
	}
}

// theBotArchive is a .tar.gz of files, as embed_gen.go writes one.
func theBotArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestTheBotArchiveIsUnpackedWhereTheDockerfilesCopyItFrom(t *testing.T) {
	fsys := fstest.MapFS{
		"docker/desktop.basic.Dockerfile": {Data: []byte("FROM scratch\n")},
		"agent/vm_server.py":              {Data: []byte("print()\n")},
		decurix.TheBotSource: {Data: theBotArchive(t, map[string]string{
			"go.mod":           "module thebot\n",
			"cmd/soul/main.go": "package main\n",
		})},
	}

	files := contextNames(t, fsys)

	for name, want := range map[string]string{
		"agent/thebot/go.mod":           "module thebot\n",
		"agent/thebot/cmd/soul/main.go": "package main\n",
		"agent/vm_server.py":            "print()\n",
	} {
		if files[name] != want {
			t.Errorf("%s = %q, want %q", name, files[name], want)
		}
	}

	if _, sent := files[decurix.TheBotSource]; sent {
		t.Errorf("the archive itself was sent as %s", decurix.TheBotSource)
	}
}

func TestATheBotArchiveCannotWriteOutsideItsFolder(t *testing.T) {
	fsys := fstest.MapFS{
		decurix.TheBotSource: {Data: theBotArchive(t, map[string]string{"../../docker/desktop.basic.Dockerfile": "FROM evil\n"})},
	}

	if err := tarBuildContext(io.Discard, fsys); err == nil {
		t.Fatal("an archive entry leaving agent/thebot was accepted")
	}
}

// What this guards: go:embed leaves out agent/thebot (a module of its own),
// so every COPY of it failed in a released binary while `docker build` from
// the repository worked. Needs `go generate .` to have run, like a release.
func TestTheEmbeddedContextHasEverythingTheDockerfilesCopy(t *testing.T) {
	if _, err := fs.Stat(decurix.DesktopBuildContext, decurix.TheBotSource); err != nil {
		t.Skipf("%s is not embedded: run `go generate .` at the repository root first", decurix.TheBotSource)
	}

	files := contextNames(t, decurix.DesktopBuildContext)

	has := func(source string) bool {
		source = strings.TrimSuffix(source, "/")
		if _, ok := files[source]; ok {
			return true
		}

		for name := range files {
			if strings.HasPrefix(name, source+"/") {
				return true
			}
		}

		return false
	}

	for _, img := range desktopImages {
		dockerfile, ok := files[img.Dockerfile]
		if !ok {
			t.Errorf("%s is not in the build context", img.Dockerfile)
			continue
		}

		for _, line := range strings.Split(dockerfile, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[0] != "COPY" {
				continue
			}

			// COPY [--flag ...] source... destination; a --from copy reads
			// another build stage, not the context.
			args := fields[1:]
			fromStage := false

			for len(args) > 0 && strings.HasPrefix(args[0], "--") {
				fromStage = fromStage || strings.HasPrefix(args[0], "--from")
				args = args[1:]
			}

			if fromStage || len(args) < 2 {
				continue
			}

			for _, source := range args[:len(args)-1] {
				if !has(source) {
					t.Errorf("%s copies %s, which is not in the embedded build context", img.Dockerfile, source)
				}
			}
		}
	}
}

// TestEveryAgentFileIsEmbedded checks the checkout's agent/ against the
// embedded context for the files go:embed leaves out of a directory without a
// word: names beginning with "_" (a "." name is a dotfile nobody builds with).
// decurix_agent/computer/__main__.py was missing, and `gui` failed in every
// desktop a released binary or the app built.
func TestEveryAgentFileIsEmbedded(t *testing.T) {
	root := filepath.Join("..", "..")

	err := filepath.WalkDir(filepath.Join(root, "agent"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		name := d.Name()

		if d.IsDir() {
			// TheBot is embedded as an archive; caches and dot folders are not sources.
			// agent/browser-agent is no image's any more (2026-11-03) and is being deleted; until it is, agent/*-*
			// still carries its other files, but nothing needs its _ names.
			if rel == decurix.TheBotDir || rel == "agent/browser-agent" || name == "__pycache__" ||
				strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasPrefix(name, "_") {
			return nil
		}

		if _, err := fs.Stat(decurix.DesktopBuildContext, rel); err != nil {
			t.Errorf("%s is not embedded: name it in embed.go's patterns", rel)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAnImageIsOutOfDateExactlyWhenWhatItIsBuiltFromChanges: a changed agent
// file makes the desktop image out of date and leaves the tools image alone; a
// changed tools Dockerfile does the opposite.
func TestAnImageIsOutOfDateExactlyWhenWhatItIsBuiltFromChanges(t *testing.T) {
	desktopImg := desktopImage{Tag: desktopImageBasic, Dockerfile: "docker/desktop.basic.Dockerfile"}
	toolsImg := desktopImage{Tag: ToolsImage, Dockerfile: "docker/tools.Dockerfile"}

	base := fstest.MapFS{
		"docker/desktop.basic.Dockerfile":          {Data: []byte("FROM x\nCOPY agent /opt\n")},
		"docker/unused.Dockerfile":                 {Data: []byte("FROM x\n")},
		"docker/tools.Dockerfile":                  {Data: []byte("FROM alpine\n")},
		"agent/decurix_agent/computer/__main__.py": {Data: []byte("main()\n")},
	}

	hashes := func(fsys fs.FS) (string, string) {
		d, err := contextHash(fsys, desktopImg)
		if err != nil {
			t.Fatal(err)
		}

		tl, err := contextHash(fsys, toolsImg)
		if err != nil {
			t.Fatal(err)
		}

		return d, tl
	}

	with := func(name, data string) fstest.MapFS {
		changed := fstest.MapFS{}
		for k, v := range base {
			changed[k] = v
		}

		changed[name] = &fstest.MapFile{Data: []byte(data)}

		return changed
	}

	d0, t0 := hashes(base)
	if again, _ := hashes(base); again != d0 {
		t.Fatal("the same sources gave two hashes")
	}

	if d, tl := hashes(with("agent/decurix_agent/computer/__main__.py", "main(1)\n")); d == d0 || tl != t0 {
		t.Errorf("an agent change: desktop changed %v (want true), tools changed %v (want false)", d != d0, tl != t0)
	}

	if d, tl := hashes(with("docker/tools.Dockerfile", "FROM alpine:3\n")); d != d0 || tl == t0 {
		t.Errorf("a tools Dockerfile change: desktop changed %v (want false), tools changed %v (want true)", d != d0, tl != t0)
	}

	if d, _ := hashes(with("docker/unused.Dockerfile", "FROM y\n")); d != d0 {
		t.Error("a Dockerfile the desktop image is not built from changed its hash")
	}
}
