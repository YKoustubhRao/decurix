package hub

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Unpack extracts a .zip, .tar, .tar.gz/.tgz or .tar.bz2 archive into destDir.
// It refuses any entry that would land outside destDir (an absolute path,
// "..", a symlink pointing up or out, a path through a symlink), unpacks into
// a temporary directory beside destDir and renames it into place only when
// every entry succeeded, so a half-unpacked directory is never left where a
// runtime would load it. An existing destDir is replaced.
func Unpack(archive, destDir string) error {
	kind, err := archiveKind(archive)
	if err != nil {
		return err
	}
	destDir = filepath.Clean(destDir)
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(destDir)+".unpack-")
	if err != nil {
		return err
	}
	placed := false
	defer func() {
		if !placed {
			_ = os.RemoveAll(tmp)
		}
	}()

	switch kind {
	case "zip":
		err = unzip(archive, tmp)
	default:
		err = untarFile(archive, kind, tmp)
	}
	if err != nil {
		return fmt.Errorf("hub: unpack %s: %w", filepath.Base(archive), err)
	}
	if err := replaceDir(tmp, destDir); err != nil {
		return fmt.Errorf("hub: unpack %s: %w", filepath.Base(archive), err)
	}
	placed = true
	return nil
}

// archiveKind names the archive format from the file name.
func archiveKind(name string) (string, error) {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return "zip", nil
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tar.gz", nil
	case strings.HasSuffix(n, ".tar.bz2"), strings.HasSuffix(n, ".tbz2"):
		return "tar.bz2", nil
	case strings.HasSuffix(n, ".tar"):
		return "tar", nil
	case strings.HasSuffix(n, ".tar.xz"), strings.HasSuffix(n, ".txz"), strings.HasSuffix(n, ".xz"),
		strings.HasSuffix(n, ".7z"), strings.HasSuffix(n, ".tar.zst"), strings.HasSuffix(n, ".zst"):
		return "", fmt.Errorf("hub: cannot unpack %s: Go's standard library has no reader for that format; use a .zip, .tar.gz or .tar.bz2 build", filepath.Base(name))
	}
	return "", fmt.Errorf("hub: cannot tell what kind of archive %s is", filepath.Base(name))
}

// replaceDir renames tmp to dest, moving an existing dest aside first and
// putting it back if the rename fails.
func replaceDir(tmp, dest string) error {
	if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
		return os.Rename(tmp, dest)
	}
	old := fmt.Sprintf("%s.old-%d", dest, time.Now().UnixNano())
	if err := os.Rename(dest, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Rename(old, dest)
		return err
	}
	return os.RemoveAll(old)
}

// entryTarget is where an archive entry goes under root, or an error when it
// would escape root. The empty path and "." map to "" (the root itself).
func entryTarget(root, name string) (string, string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if n == "" {
		return "", "", fmt.Errorf("an entry has no name")
	}
	if strings.HasPrefix(n, "/") || (len(n) >= 2 && n[1] == ':') {
		return "", "", fmt.Errorf("entry %q has an absolute path", name)
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(n, ":<>|\"?*") {
		return "", "", fmt.Errorf("entry %q has a name Windows cannot hold safely", name)
	}
	clean := path.Clean(n)
	if clean == "." {
		return "", "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("entry %q would land outside the folder", name)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("entry %q would land outside the folder", name)
	}
	if throughSymlink(root, target) {
		return "", "", fmt.Errorf("entry %q goes through a symlink", name)
	}
	return target, clean, nil
}

// throughSymlink reports whether any directory between root and target is a symlink.
func throughSymlink(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return true
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// checkLink refuses a symlink whose target is absolute or climbs with "..":
// a link without ".." can only point at or below its own directory.
func checkLink(entry, link string) error {
	l := strings.ReplaceAll(link, "\\", "/")
	if l == "" || strings.HasPrefix(l, "/") || (len(l) >= 2 && l[1] == ':') {
		return fmt.Errorf("symlink %q points at an absolute path %q", entry, link)
	}
	for _, seg := range strings.Split(l, "/") {
		if seg == ".." {
			return fmt.Errorf("symlink %q climbs out with %q", entry, link)
		}
	}
	return nil
}

// fileMode is the permission an extracted file gets: the archive's own bits
// (the executable bit kept), never setuid/setgid/sticky, always owner rw.
func fileMode(m os.FileMode) os.FileMode {
	perm := m.Perm()
	if perm == 0 {
		perm = 0o644
	}
	return perm | 0o600
}

// writeFile creates target with r's content; an existing regular file (a
// repeated entry) is replaced, anything else in the way is an error.
func writeFile(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(target); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is already there and is not a file", filepath.Base(target))
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode(mode))
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// unzip extracts a zip archive into root.
func unzip(archive, root string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target, clean, err := entryTarget(root, f.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			link, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			if err := makeSymlink(target, clean, string(link)); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeFile(target, rc, mode)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// untarFile extracts a plain, gzip or bzip2 tar archive into root.
func untarFile(archive, kind, root string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	switch kind {
	case "tar.gz":
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	case "tar.bz2":
		r = bzip2.NewReader(f)
	}
	return untar(r, root)
}

// untar extracts tar entries into root.
func untar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, clean, err := entryTarget(root, hdr.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(target, tr, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := makeSymlink(target, clean, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeLink:
			src, _, err := entryTarget(root, hdr.Linkname)
			if err != nil || src == "" {
				return fmt.Errorf("hard link %q: bad target %q", hdr.Name, hdr.Linkname)
			}
			if err := copyRegular(src, target); err != nil {
				return fmt.Errorf("hard link %q: %w", hdr.Name, err)
			}
		default:
			// Devices, fifos and the like have no place in a model or runtime archive.
		}
	}
}

// makeSymlink creates a checked symlink at target.
func makeSymlink(target, entry, link string) error {
	if err := checkLink(entry, link); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(filepath.FromSlash(link), target); err != nil {
		return fmt.Errorf("symlink %q: %w", entry, err)
	}
	return nil
}

// copyRegular copies an already extracted regular file (a hard link's source).
func copyRegular(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(src))
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFile(dst, in, fi.Mode())
}
