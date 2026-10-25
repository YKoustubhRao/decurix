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

// Open returns the store at root, creating the directory.
func Open(root string) (*Store, error) {
	if root == "" {
		root = DefaultRoot()
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
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

// WriteJSON writes v as indented JSON to name under the root, atomically.
func (s *Store) WriteJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := s.Path(name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Exists reports whether name exists under the root.
func (s *Store) Exists(name string) bool {
	_, err := os.Stat(s.Path(name))
	return !errors.Is(err, os.ErrNotExist)
}
