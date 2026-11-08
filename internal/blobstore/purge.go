// purge.go is the one place a blob's bytes change after it is stored: a leaked secret purged by value (internal/purge,
// docs/trajectory.md "Purging a leaked secret"). A secret is not a label, so plan rule 4 does not keep it.
//
// A purged blob keeps its name, the SHA-256 of the bytes it was stored with, so every row and prompt prefix that
// names it still joins (llm_calls' messages, prefix and prompt hashes, a run bundle's files). Each stored copy of the
// value is overwritten by a tombstone of the same length, so a blob's length (a bundle's File.Bytes, a tar's entry
// sizes) stays true. Beside it, <sha>.purged records the SHA-256 of the new bytes and the judgement that purged it, so
// a check that hashes a blob tells a tombstoned one from a corrupted one (Purged).

package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// purgedSuffix names the note beside a purged blob.
const purgedSuffix = ".purged"

// PurgeNote is what is kept beside a purged blob: never the value.
type PurgeNote struct {
	// SHA256 is the name the blob keeps: the hash of the bytes it was first stored with.
	SHA256 string `json:"sha256"`
	// ContentSHA256 is the hash of the bytes it holds now.
	ContentSHA256 string `json:"content_sha256"`
	Bytes         int64  `json:"bytes"`
	PurgedNS      int64  `json:"purged_ns"`
	// Judgement is the judgement that records who purged it and why.
	Judgement string `json:"judgement"`
}

// Tombstone is the n bytes a stored copy of a purged value is overwritten with: mark followed by '_' up to n, or mark
// cut to n with its last byte kept (mark is ASCII). The length is the value's, so a blob's length stays true.
func Tombstone(mark string, n int) []byte {
	if n <= 0 {
		return nil
	}
	if n >= len(mark) {
		return []byte(mark + strings.Repeat("_", n-len(mark)))
	}
	if n == 1 {
		return []byte(mark[len(mark)-1:])
	}
	return []byte(mark[:n-1] + mark[len(mark)-1:])
}

// PurgeForms walks every blob and rewrites, in place, each one holding any of forms, every copy overwritten by
// Tombstone(mark, its length); it streams each blob twice (a look, then the rewrite) and never holds one whole. each is
// told the name of every blob rewritten. A blob that cannot be rewritten (on Windows, one a reader holds open) is
// counted in failed and left as it was; the walk goes on.
func (s *Store) PurgeForms(forms []string, mark, judgement string, each func(sha string)) (failed int, err error) {
	pats := patterns(forms)
	if len(pats) == 0 {
		return 0, errors.New("blobstore: nothing to purge")
	}
	root := filepath.Join(s.root, "sha256")
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			failed++
			return nil
		}
		if d.IsDir() || !ValidSHA(d.Name()) {
			return nil
		}
		hit, err := holds(p, pats)
		if err != nil {
			failed++
			return nil
		}
		if !hit {
			return nil
		}
		if err := s.rewrite(d.Name(), pats, mark, judgement); err != nil {
			failed++
			return nil
		}
		if each != nil {
			each(d.Name())
		}
		return nil
	})
	if walkErr != nil {
		return failed, fmt.Errorf("blobstore: walking the blobs: %w", walkErr)
	}
	return failed, nil
}

// Purged is the note kept beside the blob sha when it was purged; ok is false for a blob never purged.
func (s *Store) Purged(sha string) (PurgeNote, bool) {
	if !ValidSHA(sha) {
		return PurgeNote{}, false
	}
	b, err := os.ReadFile(s.path(sha) + purgedSuffix)
	if err != nil {
		return PurgeNote{}, false
	}
	var n PurgeNote
	if json.Unmarshal(b, &n) != nil || n.SHA256 != sha {
		return PurgeNote{}, false
	}
	return n, true
}

// patterns is forms as bytes, without empty ones and repeats.
func patterns(forms []string) [][]byte {
	seen := map[string]bool{}
	var out [][]byte
	for _, f := range forms {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, []byte(f))
		}
	}
	return out
}

// holds reports whether the file at p holds any of pats.
func holds(p string, pats [][]byte) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	n, err := replaceStream(f, io.Discard, pats, "", true)
	return n > 0, err
}

// rewrite overwrites every copy of pats in the blob sha with its tombstone: the note first (so a crash between the
// two leaves a note a rerun corrects, never a changed blob with no note), then the new bytes renamed into place.
func (s *Store) rewrite(sha string, pats [][]byte, mark, judgement string) error {
	tmpDir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	src, err := os.Open(s.path(sha))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "purge-*")
	if err != nil {
		src.Close()
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once renamed
	h := sha256.New()
	counter := &countingWriter{w: io.MultiWriter(tmp, h)}
	_, err = replaceStream(src, counter, pats, mark, false)
	src.Close()
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	note, _ := json.Marshal(PurgeNote{SHA256: sha, ContentSHA256: hex.EncodeToString(h.Sum(nil)), Bytes: counter.n,
		PurgedNS: time.Now().UnixNano(), Judgement: judgement})
	if err := writeAtomic(tmpDir, s.path(sha)+purgedSuffix, note); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(sha))
}

// writeAtomic writes b to dst through a synced temp file in tmpDir.
func writeAtomic(tmpDir, dst string, b []byte) error {
	f, err := os.CreateTemp(tmpDir, "note-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

// Write writes p to the wrapped writer and counts it.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// replaceStream copies r to w with every copy of pats overwritten by Tombstone(mark, its length) and returns how many
// were found; with stopAtFirst it returns at the first. It reads in chunks and keeps only a pattern's length less one
// between them, so a copy split across two reads is still found. At one position the longest pattern wins, and a
// match is taken only once every pattern that could start at or before it is wholly read.
func replaceStream(r io.Reader, w io.Writer, pats [][]byte, mark string, stopAtFirst bool) (int, error) {
	maxLen := 0
	for _, p := range pats {
		maxLen = max(maxLen, len(p))
	}
	chunk := make([]byte, 64<<10)
	var buf []byte
	found := 0
	eof := false
	for {
		if !eof {
			n, err := r.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if err == io.EOF {
				eof = true
			} else if err != nil {
				return found, err
			}
		}
		for {
			i, l := earliest(buf, pats)
			if i < 0 || (!eof && i+maxLen > len(buf)) {
				break
			}
			found++
			if stopAtFirst {
				return found, nil
			}
			if _, err := w.Write(buf[:i]); err != nil {
				return found, err
			}
			if _, err := w.Write(Tombstone(mark, l)); err != nil {
				return found, err
			}
			buf = buf[i+l:]
		}
		if eof {
			_, err := w.Write(buf)
			return found, err
		}
		if keep := maxLen - 1; len(buf) > keep {
			if _, err := w.Write(buf[:len(buf)-keep]); err != nil {
				return found, err
			}
			buf = append(buf[:0:0], buf[len(buf)-keep:]...)
		}
	}
}

// earliest is the position and length of the first copy of any of pats in buf (the longest at that position), or -1.
func earliest(buf []byte, pats [][]byte) (int, int) {
	at, length := -1, 0
	for _, p := range pats {
		i := bytes.Index(buf, p)
		if i < 0 {
			continue
		}
		if at < 0 || i < at || (i == at && len(p) > length) {
			at, length = i, len(p)
		}
	}
	return at, length
}
