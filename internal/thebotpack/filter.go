// Package thebotpack says what of agent/thebot the TheBot archive (agent/thebot.tar.gz, made by embed_gen.go)
// carries. Its own package, importing nothing of Decurix, so the packer can use it before the archive it makes exists.
package thebotpack

import (
	"io/fs"
	"strings"
)

// Leaves reports whether the archive leaves an entry out, and whether its whole folder too. A folder is never an
// entry of its own (its files are, walked into), and testdata is skipped whole: go build reads neither _test.go files
// nor testdata, and every byte kept is in the binary and in each desktop image's build context (resource plan P1 #10).
// Anything else that is not a regular file is left out too.
func Leaves(d fs.DirEntry) (leave, folder bool) {
	if d.IsDir() {
		return true, d.Name() == "testdata"
	}
	return !d.Type().IsRegular() || strings.HasSuffix(d.Name(), "_test.go"), false
}
