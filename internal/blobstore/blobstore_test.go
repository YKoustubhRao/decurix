package blobstore

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sha, n, err := s.Put(strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || sha != SHA256([]byte("hello")) {
		t.Fatalf("Put = %s, %d", sha, n)
	}
	if !s.Has(sha) {
		t.Fatal("Has is false after Put")
	}
	r, err := s.Open(sha)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "hello" {
		t.Fatalf("Open read %q", got)
	}
	want := filepath.Join(s.Root(), "sha256", sha[:2], sha)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("blob not at %s: %v", want, err)
	}
}

func TestSecondPutDoesNotRewrite(t *testing.T) {
	s, _ := New(t.TempDir())
	sha, err := s.PutBytes([]byte("same bytes"))
	if err != nil {
		t.Fatal(err)
	}
	p := s.path(sha)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	again, err := s.PutBytes([]byte("same bytes"))
	if err != nil || again != sha {
		t.Fatalf("second Put = %s, %v", again, err)
	}
	info, _ := os.Stat(p)
	if !info.ModTime().Equal(old) {
		t.Fatalf("blob rewritten: mtime %v, want %v", info.ModTime(), old)
	}
	tmp, _ := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if len(tmp) != 0 {
		t.Fatalf("temp files left behind: %d", len(tmp))
	}
}

func TestBadSHARefused(t *testing.T) {
	s, _ := New(t.TempDir())
	for _, bad := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("g", 64), "../" + strings.Repeat("a", 61),
		strings.Repeat("a", 65)} {
		if _, err := s.Open(bad); err != ErrBadSHA {
			t.Errorf("Open(%q) = %v, want ErrBadSHA", bad, err)
		}
		if s.Has(bad) {
			t.Errorf("Has(%q) is true", bad)
		}
	}
	if s.Has(strings.Repeat("a", 64)) {
		t.Error("Has is true for a blob never stored")
	}
}

func TestConcurrentPutOfSameBytes(t *testing.T) {
	s, _ := New(t.TempDir())
	data := bytes.Repeat([]byte("frame"), 50000)
	var wg sync.WaitGroup
	shas := make([]string, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			shas[i], errs[i] = s.PutBytes(data)
		}(i)
	}
	wg.Wait()
	for i := range shas {
		if errs[i] != nil || shas[i] != SHA256(data) {
			t.Fatalf("Put %d = %s, %v", i, shas[i], errs[i])
		}
	}
	dir := filepath.Join(s.Root(), "sha256", shas[0][:2])
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d files under %s, want 1", len(entries), dir)
	}
	r, _ := s.Open(shas[0])
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("the stored blob is not the bytes put")
	}
}

// A temp file a crash left behind is cleared when the store opens.
func TestOpeningClearsACrashesTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "tmp", "put-123"), []byte("half a blob"), 0o600)
	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(left) != 0 {
		t.Fatalf("%d temp files left", len(left))
	}
}
