package stepimages

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newStore(t *testing.T, maxBytes int) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewStore(db, maxBytes)
	if err != nil {
		t.Fatal(err)
	}

	return store
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestPutStoresAJPEGUnderAnUnguessableID(t *testing.T) {
	store := newStore(t, 0)

	id, err := store.Put("box", testPNG(t, 64, 36))
	if err != nil {
		t.Fatal(err)
	}

	if len(id) != 24 {
		t.Errorf("id %q: want 24 hex characters", id)
	}

	data, ok := store.Get(id)
	if !ok {
		t.Fatal("stored image not found")
	}

	if !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
		t.Error("stored image is not a JPEG")
	}
}

func TestPutRejectsWhatIsNotAPNG(t *testing.T) {
	if _, err := newStore(t, 0).Put("box", []byte("not an image")); err == nil {
		t.Fatal("want an error for garbage input")
	}
}

// A small PNG can declare a huge canvas; the bound must be checked before
// the pixels are allocated.
func TestPutRejectsOversizedDimensionsBeforeDecoding(t *testing.T) {
	patched := append([]byte(nil), testPNG(t, 1, 1)...)

	// IHDR's width and height, then its CRC recomputed so the header is valid.
	binary.BigEndian.PutUint32(patched[16:20], 20000)
	binary.BigEndian.PutUint32(patched[20:24], 20000)
	binary.BigEndian.PutUint32(patched[29:33], crc32.ChecksumIEEE(patched[12:29]))

	if _, err := newStore(t, 0).Put("box", patched); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge for a 20000x20000 canvas", err)
	}
}

func TestStoreEvictsTheOldestImagesPastItsBound(t *testing.T) {
	probe := newStore(t, 0)
	if _, err := probe.Put("box", testPNG(t, 32, 32)); err != nil {
		t.Fatal(err)
	}
	one := probe.size

	// Room for one image and a half: the second Put must push the first out.
	store := newStore(t, one+one/2)

	first, _ := store.Put("box", testPNG(t, 32, 32))
	second, _ := store.Put("box", testPNG(t, 32, 32))

	if _, ok := store.Get(first); ok {
		t.Error("the oldest image survived past the byte bound")
	}

	if _, ok := store.Get(second); !ok {
		t.Error("the newest image was evicted instead of the oldest")
	}

	if store.size > store.maxBytes {
		t.Errorf("size %d exceeds bound %d", store.size, store.maxBytes)
	}
}

// A task record's screenshots are its evidence; they must outlive a restart,
// and the byte bound must still hold across one.
func TestImagesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlitedb.FileName)

	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(db, 0)
	if err != nil {
		t.Fatal(err)
	}

	id, err := store.Put("box", testPNG(t, 16, 16))
	if err != nil {
		t.Fatal(err)
	}

	size := store.size
	db.Close()

	db, err = sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reopened, err := NewStore(db, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := reopened.Get(id); !ok {
		t.Fatal("screenshot lost across a reopen")
	}

	if reopened.size != size {
		t.Fatalf("size after reopen = %d, want %d", reopened.size, size)
	}
}
