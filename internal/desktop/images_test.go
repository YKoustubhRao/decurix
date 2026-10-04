package desktop

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
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
