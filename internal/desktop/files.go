package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// File is one file to put into a desktop.
type File struct {
	Name string
	Data []byte
}

// HostCopyPath was a file or directory on the host to copy into a new desktop.
// Copying host paths into desktops is gone (a desktop gets one shared folder,
// chosen at the person's own screen); BeginProvisioning refuses any.
type HostCopyPath struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

// desktopUID/GID own everything the agent works on (the base image's
// "headless" user, uid 1001 - checked with `id headless` in a desktop).
const (
	desktopUID = 1001
	desktopGID = 1001
)

// CopyFiles writes files into dir inside a desktop, owned by its user,
// creating dir as needed, as that user (WriteTar). dir must be under
// /home/headless. It works on a stopped desktop too - Docker copies into a container's filesystem whether
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

	// As the worker, never through Docker's root-side copy (homefs.go).
	if err := m.WriteTar(ctx, desktopID, home, &buf); err != nil {
		return fmt.Errorf("copy files into desktop %s: %w", sanitizeName(desktopID), err)
	}

	return nil
}

// CopyOut reads a folder or file under /home/headless out of a desktop, as
// the worker (ReadTar), as a tar stream with entries named from the folder's
// own name down. Like CopyFiles it works on a stopped desktop. A path that is
// not there is ErrNotFound; the caller closes the stream.
func (m *Manager) CopyOut(ctx context.Context, desktopID, p string) (io.ReadCloser, error) {
	return m.ReadTar(ctx, desktopID, p, 0)
}

// UsesLocalModel reports whether any desktop reasons with the local Ollama
// model: TheBot on the Ollama brain, which is also what a desktop created with
// a removed harness runs (a hosted brain only embeds through Ollama, which
// never needs the chat model loaded). A desktop list that cannot be read
// counts as yes: warming a model nobody needs is cheaper than a cold one
// somebody does.
func (m *Manager) UsesLocalModel(ctx context.Context) bool {
	list, err := m.List(ctx)
	if err != nil {
		return true
	}

	for _, d := range list {
		if b := (Brain{Kind: d.Brain}); !b.Hosted() && !b.VMOnly() {
			return true
		}
	}

	return false
}

// ComputeDefaultVMDest computes the corresponding destination path inside the VM filesystem
// (/home/headless/...) for a host file or directory.
//
//  1. If the path resides inside the host user's home directory (e.g. ~/Documents/work or C:\Users\name\Desktop\repo),
//     the relative hierarchy is preserved (e.g. /home/headless/Documents/work or /home/headless/Desktop/repo).
//  2. If the path contains standard directories (e.g. Desktop, Documents, Downloads, Pictures, Music, Videos, Projects, Code),
//     that subtree is preserved under /home/headless/<subtree>.
//  3. Otherwise, it defaults to /home/headless/<basename>.
func ComputeDefaultVMDest(src string) string {
	const home = "/home/headless"

	srcClean := filepath.Clean(src)
	srcBase := filepath.Base(srcClean)
	if srcBase == "." || srcBase == "/" || srcBase == "\\" || srcBase == "" {
		return home
	}

	// 1. Check if relative to host user's home directory
	if userHome, err := os.UserHomeDir(); err == nil && userHome != "" {
		userHomeClean := filepath.Clean(userHome)
		if rel, err := filepath.Rel(userHomeClean, srcClean); err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			relSlash := filepath.ToSlash(rel)
			return path.Join(home, relSlash)
		}
	}

	// 2. Check for common folder anchors in the path (e.g. /Documents/foo or \Desktop\foo)
	slashPath := "/" + strings.Trim(filepath.ToSlash(srcClean), "/")
	commonFolders := []string{"Desktop", "Documents", "Downloads", "Pictures", "Music", "Videos", "Projects", "Code", "workspace", "work"}
	for _, folder := range commonFolders {
		pattern := "/" + strings.ToLower(folder) + "/"
		lowerPath := strings.ToLower(slashPath)
		if idx := strings.Index(lowerPath, pattern); idx != -1 {
			sub := slashPath[idx+1:]
			return path.Join(home, sub)
		}
		if strings.HasSuffix(lowerPath, "/"+strings.ToLower(folder)) {
			suffixIdx := len(lowerPath) - len(folder)
			sub := slashPath[suffixIdx:]
			return path.Join(home, sub)
		}
	}

	// 3. Fallback to basename under home
	return path.Join(home, srcBase)
}
