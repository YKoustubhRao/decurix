package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/docker/docker/api/types/container"
)

// File is one file to put into a desktop.
type File struct {
	Name string
	Data []byte
}

// HostCopyPath specifies a file or directory on the host filesystem to copy
// into the VM filesystem when creating a desktop.
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

// CopyOut reads a folder or file under /home/headless out of a desktop, as the
// tar stream Docker produces (entries named from the folder's own name down).
// Like CopyFiles it works on a stopped desktop. A path that is not there is
// ErrNotFound; the caller closes the stream.
func (m *Manager) CopyOut(ctx context.Context, desktopID, p string) (io.ReadCloser, error) {
	const home = "/home/headless"

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

// ParseHostCopyPaths parses a string containing host copy paths.
// Accepts:
// 1. JSON array of HostCopyPath objects: `[{"src":"/host/a","dest":"a"}]`
// 2. JSON array of path strings: `["/host/a -> a", "/host/b"]`
// 3. Multiline or delimiter-separated text (newlines, semicolons):
//   - `src -> dest` or `src => dest`
//   - `src:dest` (preserving Windows drive letters like C:\)
//   - `src` (destination defaults to empty, resolved under /home/headless/<basename>)
func ParseHostCopyPaths(raw string) ([]HostCopyPath, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	// Try JSON unmarshal as []HostCopyPath
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		var list []HostCopyPath
		if err := json.Unmarshal([]byte(raw), &list); err == nil && len(list) > 0 {
			var result []HostCopyPath
			for _, item := range list {
				item.Src = strings.TrimSpace(item.Src)
				item.Dest = strings.TrimSpace(item.Dest)
				if item.Src != "" {
					result = append(result, item)
				}
			}
			return result, nil
		}

		var strList []string
		if err := json.Unmarshal([]byte(raw), &strList); err == nil {
			var result []HostCopyPath
			for _, s := range strList {
				parsed := parseSinglePathEntry(s)
				if parsed.Src != "" {
					result = append(result, parsed)
				}
			}
			return result, nil
		}
	}

	var result []HostCopyPath
	lines := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ';'
	})

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parsed := parseSinglePathEntry(line)
		if parsed.Src != "" {
			result = append(result, parsed)
		}
	}

	return result, nil
}

func parseSinglePathEntry(entry string) HostCopyPath {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return HostCopyPath{}
	}

	var src, dest string
	if idx := strings.Index(entry, "->"); idx != -1 {
		src = entry[:idx]
		dest = entry[idx+2:]
	} else if idx := strings.Index(entry, "=>"); idx != -1 {
		src = entry[:idx]
		dest = entry[idx+2:]
	} else {
		// Check for colon separator, careful with Windows drive letter (e.g. C:\)
		colonIdx := -1
		for i := 0; i < len(entry); i++ {
			if entry[i] == ':' {
				if i == 1 && ((entry[0] >= 'a' && entry[0] <= 'z') || (entry[0] >= 'A' && entry[0] <= 'Z')) {
					continue
				}
				colonIdx = i
				break
			}
		}
		if colonIdx != -1 {
			src = entry[:colonIdx]
			dest = entry[colonIdx+1:]
		} else {
			src = entry
			dest = ""
		}
	}

	src = strings.Trim(strings.TrimSpace(src), `"'`)
	dest = strings.Trim(strings.TrimSpace(dest), `"'`)

	return HostCopyPath{Src: src, Dest: dest}
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

// ResolveVMDestPath computes and validates the destination path within the VM filesystem
// for a host file or directory.
//
// Destinations must reside under /home/headless.
// If dest is empty:
//   - automatically maps to the corresponding VM path via ComputeDefaultVMDest(src).
//
// If dest is relative (e.g. "Desktop/data"):
//   - resolved as /home/headless/Desktop/data
//
// If dest is "/home/headless" or "/home/headless/Desktop" or ends with "/":
//   - the source basename is placed inside dest.
func ResolveVMDestPath(src, dest string, isDir bool) (string, error) {
	const home = "/home/headless"

	srcClean := filepath.Clean(src)
	srcBase := filepath.Base(srcClean)
	if srcBase == "." || srcBase == "/" || srcBase == "\\" || srcBase == "" {
		return "", fmt.Errorf("invalid host source path %q", src)
	}

	dest = strings.TrimSpace(dest)
	dest = strings.ReplaceAll(dest, "\\", "/")

	if dest == "" {
		return ComputeDefaultVMDest(srcClean), nil
	}

	if !strings.HasPrefix(dest, "/") {
		dest = home + "/" + dest
	}

	clean := path.Clean(dest)
	if !strings.HasPrefix(clean+"/", home+"/") {
		return "", fmt.Errorf("destination %q must be under %s", dest, home)
	}

	if clean == home || clean == home+"/Desktop" || strings.HasSuffix(dest, "/") {
		return path.Join(clean, srcBase), nil
	}

	return clean, nil
}

func ensureTarDirs(tw *tar.Writer, dirRel string, seenDirs map[string]bool, now time.Time) error {
	dirRel = strings.Trim(strings.TrimSpace(dirRel), "/")
	if dirRel == "" || dirRel == "." {
		return nil
	}
	parts := strings.Split(dirRel, "/")
	curr := ""
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if curr == "" {
			curr = p
		} else {
			curr = curr + "/" + p
		}
		dirName := curr + "/"
		if !seenDirs[dirName] {
			seenDirs[dirName] = true
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     dirName,
				Mode:     0o755,
				Uid:      desktopUID,
				Gid:      desktopGID,
				ModTime:  now,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// CopyHostPaths copies the specified files and directories from the host filesystem
// into the VM filesystem for the desktop.
func (m *Manager) CopyHostPaths(ctx context.Context, desktopID string, paths []HostCopyPath) error {
	if len(paths) == 0 {
		return nil
	}

	sanitizedID := sanitizeName(desktopID)
	containerName := "desktop_" + sanitizedID

	inspect, err := m.client.ContainerInspect(ctx, containerName)
	if err != nil {
		if nf := notFound(desktopID, err); nf != err {
			return nf
		}
		return fmt.Errorf("inspect container %s: %w", containerName, err)
	}

	pr, pw := io.Pipe()
	copyOpts := container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
		CopyUIDGID:                false,
	}

	copyErrCh := make(chan error, 1)
	go func() {
		defer pr.Close()
		copyErrCh <- m.client.CopyToContainer(ctx, inspect.ID, "/home/headless", pr, copyOpts)
	}()

	tw := tar.NewWriter(pw)
	now := time.Now()
	seenDirs := make(map[string]bool)

	var writeErr error
	for _, p := range paths {
		src := strings.TrimSpace(p.Src)
		if src == "" {
			continue
		}

		fi, err := os.Stat(src)
		if err != nil {
			writeErr = fmt.Errorf("stat host path %q: %w", src, err)
			break
		}
		if privateReadinessFile(src) {
			writeErr = fmt.Errorf("readiness owner keys cannot be copied into a desktop")
			break
		}

		destVM, err := ResolveVMDestPath(src, p.Dest, fi.IsDir())
		if err != nil {
			writeErr = err
			break
		}

		rel := strings.TrimPrefix(destVM, "/home/headless/")
		if rel == "" {
			continue
		}

		if fi.IsDir() {
			if err := ensureTarDirs(tw, rel, seenDirs, now); err != nil {
				writeErr = err
				break
			}
			err := filepath.Walk(src, func(walkPath string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if privateReadinessFile(walkPath) {
					return nil
				}
				relFromSrc, err := filepath.Rel(src, walkPath)
				if err != nil {
					return err
				}
				if relFromSrc == "." {
					return nil
				}

				entryRel := path.Join(rel, filepath.ToSlash(relFromSrc))
				if info.IsDir() {
					return ensureTarDirs(tw, entryRel, seenDirs, now)
				}
				if !info.Mode().IsRegular() && (info.Mode()&os.ModeSymlink == 0) {
					return nil
				}

				if err := ensureTarDirs(tw, path.Dir(entryRel), seenDirs, now); err != nil {
					return err
				}

				if info.Mode()&os.ModeSymlink != 0 {
					target, err := os.Readlink(walkPath)
					if err != nil {
						return err
					}
					return tw.WriteHeader(&tar.Header{
						Typeflag: tar.TypeSymlink,
						Name:     entryRel,
						Linkname: filepath.ToSlash(target),
						ModTime:  now,
						Uid:      desktopUID,
						Gid:      desktopGID,
					})
				}

				if err := tw.WriteHeader(hostFileHeader(entryRel, info)); err != nil {
					return err
				}

				f, err := os.Open(walkPath)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, f)
				_ = f.Close()
				return err
			})
			if err != nil {
				writeErr = fmt.Errorf("tar directory %q: %w", src, err)
				break
			}
		} else {
			parentDir := path.Dir(rel)
			if parentDir != "." && parentDir != "" {
				if err := ensureTarDirs(tw, parentDir, seenDirs, now); err != nil {
					writeErr = err
					break
				}
			}

			if err := tw.WriteHeader(hostFileHeader(rel, fi)); err != nil {
				writeErr = err
				break
			}

			f, err := os.Open(src)
			if err != nil {
				writeErr = fmt.Errorf("open host file %q: %w", src, err)
				break
			}
			_, copyErr := io.Copy(tw, f)
			_ = f.Close()
			if copyErr != nil {
				writeErr = fmt.Errorf("read host file %q: %w", src, copyErr)
				break
			}
		}
	}

	if writeErr != nil {
		_ = tw.Close()
		_ = pw.CloseWithError(writeErr)
		<-copyErrCh
		return writeErr
	}

	if err := tw.Close(); err != nil {
		_ = pw.CloseWithError(err)
		<-copyErrCh
		return err
	}
	_ = pw.Close()

	if err := <-copyErrCh; err != nil {
		return fmt.Errorf("copy host paths into container %s: %w", containerName, err)
	}

	return nil
}

// hostFileHeader is the tar header for a regular host file copied into a desktop as name, owned by its user and
// executable when it was on the host.
func hostFileHeader(name string, info os.FileInfo) *tar.Header {
	mode := int64(0o644)
	if info.Mode()&0111 != 0 {
		mode = 0o755
	}
	return &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     info.Size(),
		Mode:     mode,
		ModTime:  info.ModTime(),
		Uid:      desktopUID,
		Gid:      desktopGID,
	}
}

// privateReadinessFile withholds an owner key even through a file symlink.
func privateReadinessFile(name string) bool {
	if strings.EqualFold(filepath.Base(name), qualification.OwnerKeyFile) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(name)
	return err == nil && strings.EqualFold(filepath.Base(resolved), qualification.OwnerKeyFile)
}
