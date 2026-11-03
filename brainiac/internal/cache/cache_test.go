package cache

import (
	"io/fs"
	"path/filepath"
	"testing"
)

// The cache is trimmed once every trimEvery writes, not on each one.
func TestTheCacheTrimsEveryFewWrites(t *testing.T) {
	c := New(t.TempDir())
	c.MaxBytes = 1
	count := func() int {
		n := 0
		filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	c.Put(Key("ocr", "m", "", []byte("a")), "a") // the first write trims
	if count() != 0 {
		t.Fatalf("the first write did not trim: %d files", count())
	}
	for i := 0; i < trimEvery-1; i++ {
		c.Put(Key("ocr", "m", "", []byte{byte(i)}), i)
	}
	if count() != trimEvery-1 {
		t.Fatalf("writes between trims walked the cache: %d files", count())
	}
	c.Put(Key("ocr", "m", "", []byte("z")), "z")
	if count() >= trimEvery {
		t.Fatalf("the next trim did not run: %d files", count())
	}
}
