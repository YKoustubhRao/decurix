package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// File is one file to put into a desktop.
type File struct {
	Name string
	Data []byte
}

// desktopUID/GID own everything the agent works on (the base image's
// "headless" user, uid 1001 - checked with `id headless` in a desktop).
const (
	desktopUID = 1001
	desktopGID = 1001
)

// CopyFiles writes files into dir inside a desktop, owned by its user,
// creating dir as needed. dir must be under /home/headless. It works on a
// stopped desktop too - Docker copies into a container's filesystem whether
// or not it runs - which is what lets an email's attachments be in place
// before the request that names them wakes the desktop up.
func (m *Manager) CopyFiles(ctx context.Context, desktopID, dir string, files []File) error {
	const home = "/home/headless"

	clean := path.Clean(dir)
	if !strings.HasPrefix(clean+"/", home+"/") || clean == home {
		return fmt.Errorf("copy files: %q is not a folder under %s", dir, home)
	}

	rel := strings.TrimPrefix(clean, home+"/")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()

	// Every folder on the way, so a new one is owned by the desktop's user.
	parts := strings.Split(rel, "/")
	for i := range parts {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: strings.Join(parts[:i+1], "/") + "/",
			Mode: 0o755, Uid: desktopUID, Gid: desktopGID, ModTime: now,
		}); err != nil {
			return err
		}
	}

	for _, f := range files {
		name := path.Base(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || name == "/" || name == ".." || name == "" {
			return fmt.Errorf("copy files: %q is not a file name", f.Name)
		}

		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: rel + "/" + name, Size: int64(len(f.Data)),
			Mode: 0o644, Uid: desktopUID, Gid: desktopGID, ModTime: now,
		}); err != nil {
			return err
		}

		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}

	containerName := "desktop_" + sanitizeName(desktopID)

	if err := m.client.CopyToContainer(ctx, containerName, home, &buf, container.CopyToContainerOptions{}); err != nil {
		if nf := notFound(desktopID, err); nf != err {
			return nf
		}

		return fmt.Errorf("copy files into %s: %w", containerName, err)
	}

	return nil
}

// UsesLocalModel reports whether any desktop runs an engine that reads the
// local Ollama model (ollama or thebot). A desktop list that cannot be read
// counts as yes: warming a model nobody needs is cheaper than a cold one
// somebody does.
func (m *Manager) UsesLocalModel(ctx context.Context) bool {
	list, err := m.List(ctx)
	if err != nil {
		return true
	}

	for _, d := range list {
		if d.Harness == HarnessOllama || d.Harness == HarnessTheBot {
			return true
		}
	}

	return false
}
