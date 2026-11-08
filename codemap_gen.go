//go:build ignore

// Writes CODE_MAP.md at the repository root from every tracked file's own header comment (internal/codemap). Run by
// `go run codemap_gen.go`, or `go generate .`, after adding, removing or re-describing a file;
// TestCodeMapIsCurrent fails until it is run.
package main

import (
	"log"
	"os"

	"github.com/YKoustubhRao/decurix/internal/codemap"
)

// main regenerates the map in the current folder, which must be the repository root.
func main() {
	files, err := codemap.TrackedFiles(".")
	if err != nil {
		log.Fatal(err)
	}
	out, err := codemap.Generate(".", files)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(codemap.Output, out, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%d files)", codemap.Output, len(files))
}
