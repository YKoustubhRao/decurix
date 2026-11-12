package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

// Results hold text read from screens and documents: owner-only folders and
// files.
func TestModeCacheIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores Unix file modes")
	}
	c := New(filepath.Join(t.TempDir(), "cache"))
	key := Key("ocr", "m", "", []byte("screen"))
	c.Put(key, "a secret on screen")
	for p, want := range map[string]os.FileMode{c.Dir: 0o700, filepath.Dir(c.path(key)): 0o700, c.path(key): 0o600} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s is %o, want %o", p, st.Mode().Perm(), want)
		}
	}
}
