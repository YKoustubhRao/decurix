//go:build ignore

// Packs agent/thebot into agent/thebot.tar.gz, which embed.go's "agent"
// pattern then carries into the binary. Run by `go generate .` at the module
// root, before `go build`, by everything that builds a binary to hand out.
//
// agent/thebot is a Go module of its own, and go:embed leaves out any
// directory holding a go.mod: without this archive the embedded build context
// has no TheBot, and `decurix --build-images` fails at the Dockerfile's first
// COPY of it.
package main

import (
	"archive/tar"
	"compress/gzip"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"
)

const (
	source  = "agent/thebot"
	archive = "agent/thebot.tar.gz"
)

// parts are what the desktop Dockerfiles COPY out of agent/thebot.
var parts = []string{"go.mod", "go.sum", "cmd", "internal", "web", "scripts"}

// main writes the archive: every regular file of parts, named relative to
// agent/thebot, in walk order and with a fixed time so the same sources give
// the same bytes.
func main() {
	out, err := os.Create(filepath.FromSlash(archive))
	if err != nil {
		log.Fatal(err)
	}

	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)
	files := 0

	for _, part := range parts {
		root := filepath.Join(filepath.FromSlash(source), part)

		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}

			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}

			rel, err := filepath.Rel(filepath.FromSlash(source), p)
			if err != nil {
				return err
			}

			header := &tar.Header{
				Name:    path.Clean(filepath.ToSlash(rel)),
				Mode:    0o644,
				Size:    int64(len(data)),
				ModTime: time.Unix(0, 0),
			}
			if err := tw.WriteHeader(header); err != nil {
				return err
			}

			files++
			_, err = tw.Write(data)

			return err
		})
		if err != nil {
			log.Fatal(err)
		}
	}

	for _, c := range []interface{ Close() error }{tw, zw, out} {
		if err := c.Close(); err != nil {
			log.Fatal(err)
		}
	}

	log.Printf("%s: %d files", archive, files)

	buildSoul()
}

// soul is TheBot's engine, compiled here for the desktops (linux/amd64) and
// carried in the build context, so a computer building the desktop image does
// not pull the Go image (~900 MB), download TheBot's modules and compile it -
// minutes of every first start of the app (internal/desktop.prebuiltSoul).
// The Dockerfiles still compile it from source when it is not there.
const soul = "agent/thebot-soul"

func buildSoul() {
	target, err := filepath.Abs(filepath.FromSlash(soul))
	if err != nil {
		log.Fatal(err)
	}

	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", target, "./cmd/soul")
	cmd.Dir = filepath.FromSlash(source)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("build %s: %v", soul, err)
	}

	log.Printf("%s: built for linux/amd64", soul)
}
