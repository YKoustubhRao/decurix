package decurix

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The TheBot archive carries no tests (resource plan P1 #10): go build reads neither _test.go files nor testdata, and
// the archive is in the binary and in every desktop image's build context. An archive older than its packer
// (embed_gen.go) was made by an earlier one and is not judged: `go generate .` makes it again.
func TestTheBotArchiveCarriesNoTests(t *testing.T) {
	archive, err := os.Stat(filepath.FromSlash(TheBotSource))
	if err != nil {
		t.Skip("no agent/thebot.tar.gz here: go generate . makes it")
	}
	if packer, err := os.Stat("embed_gen.go"); err == nil && archive.ModTime().Before(packer.ModTime()) {
		t.Skip("agent/thebot.tar.gz predates embed_gen.go: go generate . makes it again")
	}
	f, err := os.Open(filepath.FromSlash(TheBotSource))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(h.Name, "_test.go") || strings.Contains("/"+h.Name+"/", "/testdata/") {
			t.Errorf("the archive carries %s", h.Name)
		}
	}
}
