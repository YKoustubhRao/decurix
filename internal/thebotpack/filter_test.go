package thebotpack

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The archive keeps a package's sources and leaves out its tests and their testdata, at any depth (review RES-CP-1:
// the archive test skipped until go generate ran; this checks the packer's own filter on a tree of its own).
func TestTheArchiveLeavesTestsOut(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"cmd/soul/main.go", "internal/a/a.go", "internal/a/a_test.go", "internal/a/testdata/x.json",
		"internal/b/testdata/deep/y.txt", "web/index.html", "internal/b/b_test.go"} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var kept []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		if leave, folder := Leaves(d); leave {
			if folder {
				return filepath.SkipDir
			}
			return nil
		}
		// What is kept is read as a file (embed_gen.go), so only regular files may be.
		if !d.Type().IsRegular() {
			t.Errorf("kept %s, which is not a file", p)
		}
		rel, _ := filepath.Rel(root, p)
		kept = append(kept, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(kept)
	if want := []string{"cmd/soul/main.go", "internal/a/a.go", "web/index.html"}; !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
}
