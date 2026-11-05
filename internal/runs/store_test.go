package runs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// memBlobs is an in-memory Blobs for tests, counting the Puts that stored new bytes.
type memBlobs struct {
	mu     sync.Mutex
	blobs  map[string][]byte
	stored int
}

func newMemBlobs() *memBlobs { return &memBlobs{blobs: map[string][]byte{}} }

func (m *memBlobs) Put(r io.Reader) (string, int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	sha, err := m.PutBytes(b)
	return sha, int64(len(b)), err
}

func (m *memBlobs) PutBytes(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[sha]; !ok {
		m.blobs[sha] = append([]byte(nil), b...)
		m.stored++
	}
	return sha, nil
}

func (m *memBlobs) Open(sha string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[sha]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memBlobs) Has(sha string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[sha]
	return ok
}

func (m *memBlobs) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stored
}

func newTestStore(t *testing.T) (*Store, *memBlobs) {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs := newMemBlobs()
	s, err := NewStore(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	return s, blobs
}

func TestNewStoreNeedsBlobs(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := NewStore(db, nil); !errors.Is(err, ErrNoBlobs) {
		t.Fatalf("NewStore(nil blobs) = %v, want ErrNoBlobs", err)
	}
}

func TestSaveBundleRoundTripAndUpsert(t *testing.T) {
	s, _ := newTestStore(t)
	if ok, err := s.Has("e_1"); err != nil || ok {
		t.Fatalf("Has before save = %v, %v", ok, err)
	}
	if _, ok, err := s.Bundle("e_1"); err != nil || ok {
		t.Fatalf("Bundle before save = %v, %v", ok, err)
	}

	first := Bundle{ExecID: "e_1", TaskID: "t_1", Worker: "eve", PulledAt: time.Unix(100, 0),
		Files: []File{{Path: "b", SHA256: "bb", Bytes: 2}, {Path: "a", SHA256: "aa", Bytes: 1}}}
	if err := s.SaveBundle(first); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Bundle("e_1")
	if err != nil || !ok {
		t.Fatalf("Bundle = %v, %v", ok, err)
	}
	if got.TaskID != "t_1" || got.Worker != "eve" || got.TotalBytes != 3 || len(got.Files) != 2 || got.Files[0].Path != "a" {
		t.Fatalf("bundle read back wrong: %+v", got)
	}
	if got.Missing == nil || len(got.Missing) != 0 {
		t.Fatalf("Missing = %#v, want []", got.Missing)
	}
	if got.ManifestSHA != ManifestSHA(first.Files) {
		t.Fatalf("manifest sha %q, want %q", got.ManifestSHA, ManifestSHA(first.Files))
	}
	// The manifest does not depend on the order the files were listed in.
	if ManifestSHA([]File{first.Files[1], first.Files[0]}) != got.ManifestSHA {
		t.Fatal("manifest sha depends on file order")
	}

	// A later pull of the same execution replaces the row.
	later := first
	later.Files = append(later.Files, File{Path: "c", SHA256: "cc", Bytes: 4})
	later.Missing = []string{"soul_trace: not present"}
	if err := s.SaveBundle(later); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Bundle("e_1")
	if len(got.Files) != 3 || got.TotalBytes != 7 || len(got.Missing) != 1 {
		t.Fatalf("upsert did not replace: %+v", got)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM run_bundles`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want 1", n, err)
	}
}

func TestSaveBundleNeedsIDs(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.SaveBundle(Bundle{TaskID: "t"}); err == nil {
		t.Fatal("a bundle with no execution was saved")
	}
}
