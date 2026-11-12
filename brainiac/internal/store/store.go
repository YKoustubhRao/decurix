// Package store is where Brainiac keeps what it downloads and decides, under
// one root (BRAINIAC_HOME, default ~/.brainiac):
//
//	models/hf/<org>__<repo>/<file>           Hugging Face files
//	models/github/<org>__<repo>/<tag>/<asset> GitHub release assets (archives unpacked beside them)
//	bin/llama.cpp/<tag>-<variant>/           the llama-server build
//	py/                                       the worker's Python venv
//	cache/  traces/                           perception results and request traces
//	census.json plan.json lock.json config.json
//
// Depends on catalog only.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"brainiac/internal/catalog"
)

// Store is one Brainiac home directory.
type Store struct {
	Root string
}

// DefaultRoot is BRAINIAC_HOME, or ~/.brainiac.
func DefaultRoot() string {
	if v := strings.TrimSpace(os.Getenv("BRAINIAC_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".brainiac"
	}
	return filepath.Join(home, ".brainiac")
}

// Open returns the store at root, creating the directory owner-only (0700):
// the cache holds text read from screens and documents. An existing root is
// tightened only when it is clearly Brainiac's (privateDirs).
func Open(root string) (*Store, error) {
	if root == "" {
		root = DefaultRoot()
	}
	_, err := os.Stat(root)
	fresh := errors.Is(err, os.ErrNotExist)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Root: root}
	if fresh || s.isHome() {
		s.privateDirs()
	}
	return s, nil
}

// homeMarkers are files only a Brainiac home holds.
var homeMarkers = []string{"lock.json", "plan.json", "census.json"}

// isHome reports whether the root is clearly a Brainiac home: it holds one of
// Brainiac's own files, or it is the built-in ~/.brainiac. A directory a
// person pointed BRAINIAC_HOME at is not chmodded until Brainiac has made it
// its own.
func (s *Store) isHome() bool {
	for _, m := range homeMarkers {
		if st, err := os.Lstat(s.Path(m)); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	a, err1 := filepath.Abs(s.Root)
	b, err2 := filepath.Abs(filepath.Join(home, ".brainiac"))
	return err1 == nil && err2 == nil && a == b
}

// privateParts are the folders under the root that hold what requests
// carried (perception results, traces, model logs, worker temp files).
var privateParts = []string{"cache", "traces", "logs", "tmp"}

// privateDirs takes group and other access away from the root and the
// folders in privateParts that exist. A symlink is left alone; failures are
// ignored (a root on a filesystem without modes).
func (s *Store) privateDirs() {
	tighten(s.Root, true)
	for _, d := range privateParts {
		tighten(s.Path(d), false)
	}
}

// tighten clears a directory's group and other bits; follow says whether a
// symlink to it is followed.
func tighten(dir string, follow bool) {
	stat := os.Lstat
	if follow {
		stat = os.Stat
	}
	st, err := stat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o077 == 0 {
		return
	}
	_ = os.Chmod(dir, st.Mode().Perm()&^0o077)
}

// Path joins parts under the root.
func (s *Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func flat(repo string) string { return strings.ReplaceAll(repo, "/", "__") }

// HFFile is where one Hugging Face file lives.
func (s *Store) HFFile(repo, file string) string {
	return s.Path("models", "hf", flat(repo), filepath.FromSlash(file))
}

// GitHubAsset is where a release asset is downloaded.
func (s *Store) GitHubAsset(repo, tag, asset string) string {
	return s.Path("models", "github", flat(repo), tag, asset)
}

// GitHubUnpacked is the directory an archive asset is unpacked into.
func (s *Store) GitHubUnpacked(repo, tag, asset string) string {
	return s.Path("models", "github", flat(repo), tag, "unpacked", ArchiveStem(asset))
}

// LlamaCppDir is where one llama.cpp build is unpacked.
func (s *Store) LlamaCppDir(tag, variant string) string {
	return s.Path("bin", "llama.cpp", tag+"-"+variant)
}

// Venv is the worker's Python virtual environment.
func (s *Store) Venv() string { return s.Path("py") }

// ArchiveStem is an archive name without its archive extension.
func ArchiveStem(name string) string {
	for _, ext := range []string{".tar.bz2", ".tar.gz", ".tgz", ".tar.xz", ".tar", ".zip"} {
		if strings.HasSuffix(name, ext) {
			return strings.TrimSuffix(name, ext)
		}
	}
	return name
}

// SourceFiles maps each file role of a source to where it is (or will be) on
// disk. Ollama, pip and system sources have no files of their own.
func (s *Store) SourceFiles(src catalog.Source) map[string]string {
	out := map[string]string{}
	switch src.Kind {
	case catalog.SourceHF:
		for _, f := range src.Files {
			out[f.Role] = s.HFFile(src.Repo, f.Path)
		}
	case catalog.SourceGitHub:
		if src.Unpack {
			dir := s.GitHubUnpacked(src.Repo, src.Tag, src.Asset)
			for _, f := range src.Files {
				out[f.Role] = filepath.Join(dir, filepath.FromSlash(f.Path))
			}
		} else {
			for _, f := range src.Files {
				out[f.Role] = s.GitHubAsset(src.Repo, src.Tag, src.Asset)
			}
		}
	}
	return out
}

// HasFiles reports whether every file a source needs is already on disk.
// It is false for sources without files (Ollama, pip, system).
func (s *Store) HasFiles(src catalog.Source) bool {
	files := s.SourceFiles(src)
	if len(files) == 0 {
		return false
	}
	for _, p := range files {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// VenvReady reports whether the worker venv has been set up with every
// requirement group in groups (the stamp files EnsureVenv leaves).
func (s *Store) VenvReady(groups []string) bool {
	for _, g := range groups {
		if _, err := os.Stat(filepath.Join(s.Venv(), "brainiac-"+g+".ok")); err != nil {
			return false
		}
	}
	return len(groups) > 0
}

// ReadJSON reads name under the root into v; a missing file is os.ErrNotExist.
func (s *Store) ReadJSON(name string, v any) error {
	data, err := os.ReadFile(s.Path(name))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// WriteJSON writes v as indented JSON to name under the root, atomically and
// owner-only (0600).
func (s *Store) WriteJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := s.Path(name)
	tmp := path + ".tmp"
	_ = os.Remove(tmp) // a leftover keeps its old mode through WriteFile
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Exists reports whether name exists under the root.
func (s *Store) Exists(name string) bool {
	_, err := os.Stat(s.Path(name))
	return !errors.Is(err, os.ErrNotExist)
}
