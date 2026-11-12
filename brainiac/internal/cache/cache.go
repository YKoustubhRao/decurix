// Package cache keeps perception results by content: the same picture or
// recording read by the same model with the same settings is read once. A
// conversation resends its earlier pictures every turn; without this each
// turn would OCR them again.
//
// Depends on nothing internal.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Cache is a directory of JSON results named by the hash of what made them.
type Cache struct {
	Dir      string
	MaxBytes int64 // default 256 MB
	mu       sync.Mutex
	puts     atomic.Int64
}

// New returns a cache in dir.
func New(dir string) *Cache { return &Cache{Dir: dir, MaxBytes: 256 << 20} }

// Key hashes the parts that decide a result: the operation, the model and
// its settings, and the input bytes.
func Key(op, model string, params string, input []byte) string {
	h := sha256.New()
	for _, s := range []string{op, model, params} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	h.Write(input)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Cache) path(key string) string {
	return filepath.Join(c.Dir, key[:2], key+".json")
}

// Get reads a result into v; false when absent or unreadable.
func (c *Cache) Get(key string, v any) bool {
	if c == nil || len(key) < 2 {
		return false
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// Put stores a result. A failure only costs a re-read later, so it is ignored.
func (c *Cache) Put(key string, v any) {
	if c == nil || len(key) < 2 {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	p := c.path(key)
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
	// Walking the whole cache costs O(files); once every trimEvery writes
	// keeps it near its size without paying that on every OCR.
	if c.puts.Add(1)%trimEvery == 1 {
		c.trim()
	}
}

// trimEvery is how many writes pass between trims (the first write trims).
const trimEvery = 32

// trim removes the oldest results once the cache is over MaxBytes.
func (c *Cache) trim() {
	c.mu.Lock()
	defer c.mu.Unlock()
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var all []entry
	var total int64
	_ = filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			all = append(all, entry{p, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	max := c.MaxBytes
	if max <= 0 {
		max = 256 << 20
	}
	if total <= max {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, e := range all {
		if total <= max*8/10 {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
}
