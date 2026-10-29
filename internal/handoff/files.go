package handoff

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// MaxCarryBytes bounds what one handoff carries between workers, all files together.
const MaxCarryBytes = 64 << 20

// ErrTooLarge is a set of files over MaxCarryBytes.
var ErrTooLarge = errors.New("handoff: files too large to carry")

// ReadTarFiles reads the regular files of a tar stream (Docker's copy-out
// format) as Files named by their base name, refusing more than budget bytes
// in all. Folders, links and devices are skipped: a handoff carries file
// contents, never a link that could lead elsewhere on the receiver.
func ReadTarFiles(r io.Reader, budget int64) ([]File, error) {
	tr := tar.NewReader(r)
	var out []File

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("handoff: reading files: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := path.Base(strings.ReplaceAll(hdr.Name, "\\", "/"))
		if name == "." || name == "/" || name == ".." || name == "" {
			continue
		}
		if hdr.Size > budget {
			return nil, ErrTooLarge
		}

		data, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return nil, fmt.Errorf("handoff: reading %s: %w", name, err)
		}
		budget -= int64(len(data))
		out = append(out, File{Name: name, Data: data})
	}
}
