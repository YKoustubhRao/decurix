// Package blobstore is a content-addressed file store: every blob is kept once, under the SHA-256 of its bytes, at
// <root>/sha256/<first two hex digits>/<64 hex digits>. A blob is written to a temp file, synced and renamed into
// place, so a reader never sees half a blob; one already there is never rewritten. Nothing here deletes a blob (plan
// rule 4: nothing that labels a run is deleted).
//
// The control plane keeps its blobs under sqlitedb.DataDir()/blobs: model-call messages and answers (llmrec) and the
// files of run bundles (runs). The store is host-only; no route serves it without humansOnly.
//
// Depends on the standard library only.
package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// ErrBadSHA is a SHA that is not 64 lower-case hex digits.
var ErrBadSHA = errors.New("blobstore: not a sha256 in lower-case hex")

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA reports whether sha is 64 lower-case hex digits.
func ValidSHA(sha string) bool { return shaPattern.MatchString(sha) }

// Store keeps blobs under one root directory.
type Store struct {
	root string
}

// New opens (creating if needed) a store rooted at dir.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blobstore: no directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "sha256"), 0o700); err != nil {
		return nil, fmt.Errorf("blobstore: %w", err)
	}
	return &Store{root: dir}, nil
}

// Root is the store's directory.
func (s *Store) Root() string { return s.root }

// path is where the blob sha lives. The caller has validated sha.
func (s *Store) path(sha string) string {
	return filepath.Join(s.root, "sha256", sha[:2], sha)
}

// Put stores everything r yields and returns its SHA-256 and length. A blob already stored is left as it is (its
// file is not rewritten, so its modification time does not change).
func (s *Store) Put(r io.Reader) (string, int64, error) {
	tmpDir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", 0, fmt.Errorf("blobstore: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "put-*")
	if err != nil {
		return "", 0, fmt.Errorf("blobstore: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once renamed

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		return "", 0, fmt.Errorf("blobstore: writing: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, fmt.Errorf("blobstore: syncing: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("blobstore: %w", err)
	}

	sha := hex.EncodeToString(h.Sum(nil))
	final := s.path(sha)
	if _, err := os.Stat(final); err == nil {
		return sha, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return "", 0, fmt.Errorf("blobstore: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		// Another Put of the same bytes may have won the race (Windows refuses to rename over an existing file):
		// the blob is there either way, and its bytes are the same.
		if _, statErr := os.Stat(final); statErr == nil {
			return sha, n, nil
		}
		return "", 0, fmt.Errorf("blobstore: %w", err)
	}
	return sha, n, nil
}

// PutBytes stores b and returns its SHA-256.
func (s *Store) PutBytes(b []byte) (string, error) {
	sha, _, err := s.Put(bytes.NewReader(b))
	return sha, err
}

// Open opens the blob sha for reading.
func (s *Store) Open(sha string) (io.ReadCloser, error) {
	if !ValidSHA(sha) {
		return nil, ErrBadSHA
	}
	f, err := os.Open(s.path(sha))
	if err != nil {
		return nil, fmt.Errorf("blobstore: %w", err)
	}
	return f, nil
}

// Has reports whether the blob sha is stored.
func (s *Store) Has(sha string) bool {
	if !ValidSHA(sha) {
		return false
	}
	info, err := os.Stat(s.path(sha))
	return err == nil && info.Mode().IsRegular()
}

// SHA256 is the lower-case hex SHA-256 of b: the name a blob of b has.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
