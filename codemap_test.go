package decurix

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/codemap"
)

// CODE_MAP.md is what the generator writes for the tracked files as they are now: a file added, removed or
// re-described without `go run codemap_gen.go` fails here, so the map never drifts from the code again.
func TestCodeMapIsCurrent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git: the map is built from git ls-files")
	}
	if _, err := os.Stat(".git"); err != nil {
		t.Skip("not a git checkout")
	}
	files, err := codemap.TrackedFiles(".")
	if err != nil {
		t.Skip(err)
	}
	want, err := codemap.Generate(".", files)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(codemap.Output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatalf("%s is stale: run `go run codemap_gen.go` at the repository root", codemap.Output)
	}
}
