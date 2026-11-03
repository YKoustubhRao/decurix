package sqlitedb

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// InstanceFile is the file in the data directory that names this control plane among others on the same computer or
// Docker engine: a random ID made once, the first time it is needed.
const InstanceFile = "instance-id"

// InstanceID is this control plane's instance tag, twelve hex digits: what its host-app helpers and hop containers
// carry, so that a sweep at start ends only what this instance started (review RW1-C-03). It is read from
// InstanceFile in the data directory, made there once when missing. A hash of the data directory's path was used
// before, and every containerised control plane has the same path (/data), so all of them shared one tag; the path's
// hash is still the fallback when the file can be neither read nor written, with a log line.
var InstanceID = sync.OnceValue(func() string { return instanceIDIn(DataDir()) })

// instanceIDIn is InstanceID for data directory dir.
func instanceIDIn(dir string) string {
	path := filepath.Join(dir, InstanceFile)
	if id, ok := readInstanceID(path); ok {
		return id
	}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err == nil {
			id := hex.EncodeToString(raw)
			// Made once: an exclusive create, which fails if another process made the file first (whose ID is then
			// used, once it has written it). Unlike a hard link it works on every filesystem a data directory may be
			// on, so the path's hash is no longer the fallback where links are not supported (REVIEW-3 follow-up 8).
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			switch {
			case err == nil:
				_, werr := f.WriteString(id + "\n")
				if cerr := f.Close(); werr == nil && cerr == nil {
					return id
				}
				_ = os.Remove(path)
			case errors.Is(err, os.ErrExist):
				if got, ok := awaitInstanceID(path); ok {
					return got
				}
			}
		}
	}
	abs := dir
	if a, err := filepath.Abs(dir); err == nil {
		abs = a
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	id := hex.EncodeToString(sum[:6])
	log.Printf("instance: could not keep %s in %s; this control plane is tagged by its data directory's path (%s), which another one with the same path shares", InstanceFile, dir, id)
	return id
}

// awaitInstanceID reads the instance ID another process has just created, giving it a moment to write the twelve
// digits after its exclusive create.
func awaitInstanceID(path string) (string, bool) {
	for i := 0; i < 20; i++ {
		if id, ok := readInstanceID(path); ok {
			return id, true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return "", false
}

// readInstanceID reads an instance ID file, accepting only twelve hex digits.
func readInstanceID(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(raw))
	if len(id) != 12 {
		return "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", false
	}
	return strings.ToLower(id), true
}
