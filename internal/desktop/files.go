package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
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

	if m.native != nil {
		return m.native.copyFiles(sanitizeName(desktopID), dir, files)
	}

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

// AgentPath is a path under /home/headless as the desktop's own agent names
// it: unchanged in a container, the real folder for a native worker (whose
// home is a folder of this computer). What CopyFiles wrote is told to the
// agent by this name; a path outside the home is returned as given.
func (m *Manager) AgentPath(desktopID, p string) string {
	if m.native == nil {
		return p
	}

	real, err := m.native.homePath(sanitizeName(desktopID), p)
	if err != nil {
		return p
	}

	return real
}

// CopyOut reads a folder or file under /home/headless out of a desktop, as the
// tar stream Docker produces (entries named from the folder's own name down).
// Like CopyFiles it works on a stopped desktop. A path that is not there is
// ErrNotFound; the caller closes the stream.
func (m *Manager) CopyOut(ctx context.Context, desktopID, p string) (io.ReadCloser, error) {
	const home = "/home/headless"

	if m.native != nil {
		return m.native.copyOut(sanitizeName(desktopID), p)
	}

	clean := path.Clean(p)
	if !strings.HasPrefix(clean+"/", home+"/") || clean == home {
		return nil, fmt.Errorf("copy out: %q is not under %s", p, home)
	}

	containerName := "desktop_" + sanitizeName(desktopID)

	rc, _, err := m.client.CopyFromContainer(ctx, containerName, clean)
	if err != nil {
		if nf := notFound(desktopID, err); nf != err {
			return nil, nf
		}
		if strings.Contains(strings.ToLower(err.Error()), "could not find the file") {
			return nil, ErrNotFound
		}

		return nil, fmt.Errorf("copy %s out of %s: %w", clean, containerName, err)
	}

	return rc, nil
}

// UsesLocalModel reports whether any desktop reasons with the local Ollama
// model: the ollama harness, or TheBot on the Ollama brain (a hosted brain
// only embeds through Ollama, which never needs the chat model loaded). A
// desktop list that cannot be read
// counts as yes: warming a model nobody needs is cheaper than a cold one
// somebody does.
func (m *Manager) UsesLocalModel(ctx context.Context) bool {
	list, err := m.List(ctx)
	if err != nil {
		return true
	}

	for _, d := range list {
		if d.Harness == HarnessOllama || (d.Harness == HarnessTheBot && d.Brain != BrainClaude && d.Brain != BrainAzure) {
			return true
		}
	}

	return false
}
