package grounder

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// pngHeader is the start of a PNG that declares width x height and holds no pixels: what DecodeConfig reads.
func pngHeader(width, height uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], width)
	binary.BigEndian.PutUint32(ihdr[4:], height)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

// A screenshot declaring more pixels than a screen is refused before it is decoded (C-12): nothing is allocated for
// its pixels, whatever size the caller claims.
func TestAHugeDeclaredScreenshotIsRefusedBeforeDecoding(t *testing.T) {
	data := pngHeader(30000, 30000)
	if _, _, _, err := Shrink(data, 100, 100, 50); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Shrink of a 30000x30000 declaration = %v, want ErrTooLarge", err)
	}
	if !TooLarge(30000, 30000) || TooLarge(1920, 1080) || !TooLarge(0, 10) {
		t.Fatal("TooLarge misjudges a size")
	}
}
