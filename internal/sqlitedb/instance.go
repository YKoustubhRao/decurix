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
			// Made once, without a window where a half-written file is read: an exclusive create of a temp file,
			// then a hard link that fails if another process made the file first (whose ID is then used).
			tmp, err := os.CreateTemp(dir, ".instance-id-*")
			if err == nil {
				_, werr := tmp.WriteString(id + "\n")
				cerr := tmp.Close()
				if werr == nil && cerr == nil {
					_ = os.Chmod(tmp.Name(), 0o600)
					lerr := os.Link(tmp.Name(), path)
					_ = os.Remove(tmp.Name())
					if lerr == nil || errors.Is(lerr, os.ErrExist) {
						if got, ok := readInstanceID(path); ok {
							return got
						}
					}
				} else {
					_ = os.Remove(tmp.Name())
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
