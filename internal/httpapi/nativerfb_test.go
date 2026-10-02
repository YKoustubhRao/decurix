package httpapi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// A picture is handed to the viewer as it came from the helper: a JPEG inside
// the Tight encoding's JPEG rectangle, whose length is written in one to three
// bytes of seven bits each.
func TestAJPEGIsWrappedAsATightRectangle(t *testing.T) {
	for _, n := range []int{1, 127, 128, 16383, 16384, 300000} {
		jpeg := bytes.Repeat([]byte{0xAB}, n)
		got := rfbTightJPEG(jpeg)

		if got[0] != 0x90 {
			t.Fatalf("%d bytes: the control byte is %#x, want 0x90 (JPEG)", n, got[0])
		}

		// Read the length back the way a Tight decoder does.
		length, at := int(got[1]&0x7f), 2
		if got[1]&0x80 != 0 {
			length |= int(got[2]&0x7f) << 7
			at = 3
			if got[2]&0x80 != 0 {
				length |= int(got[3]) << 14
				at = 4
			}
		}
		if length != n || !bytes.Equal(got[at:], jpeg) {
			t.Errorf("%d bytes: a decoder would read %d, with %d bytes after the header", n, length, len(got)-at)
		}
	}

	rect := rfbRect(16, 32, 640, 480, rfbEncodingTight, []byte{1, 2})
	if binary.BigEndian.Uint16(rect[0:]) != 16 || binary.BigEndian.Uint16(rect[2:]) != 32 || binary.BigEndian.Uint16(rect[4:]) != 640 ||
		binary.BigEndian.Uint16(rect[6:]) != 480 || int32(binary.BigEndian.Uint32(rect[8:])) != rfbEncodingTight || len(rect) != 14 {
		t.Errorf("rectangle header = % x", rect)
	}
}

// The pointer the viewer is given: every row as wide as the first, and a
// rectangle holding four bytes a pixel followed by one mask bit a pixel.
func TestTheArrowCursorIsAWellFormedRectangle(t *testing.T) {
	w, h := len(arrow[0]), len(arrow)
	for i, row := range arrow {
		if len(row) != w {
			t.Fatalf("row %d of the arrow is %d wide, the first is %d", i, len(row), w)
		}
	}

	rect := rfbArrowCursor()
	if want := 12 + w*h*4 + (w+7)/8*h; len(rect) != want {
		t.Fatalf("the cursor rectangle is %d bytes, want %d", len(rect), want)
	}
	if int32(binary.BigEndian.Uint32(rect[8:])) != rfbEncodingCursor {
		t.Error("the cursor rectangle is not marked as a cursor")
	}
	// Its tip is the top-left pixel: drawn, and part of the outline (black).
	if mask := rect[12+w*h*4]; mask&0x80 == 0 {
		t.Error("the arrow's tip is not drawn")
	}
}

// Of a run of mouse moves only the last is kept; a press between them is not
// lost, nor are the moves either side of it merged across it.
func TestOnlyTheLastOfARunOfMovesIsKept(t *testing.T) {
	q := newRFBInputs()
	q.push(rfbInput{kind: rfbPointer, x: 1})
	q.push(rfbInput{kind: rfbPointer, x: 2})
	q.push(rfbInput{kind: rfbPointer, x: 3, buttons: 1})
	q.push(rfbInput{kind: rfbPointer, x: 4, buttons: 1})
	q.push(rfbInput{kind: rfbPointer, x: 5})
	q.push(rfbInput{kind: rfbKey, keysym: 'a', down: true})
	q.push(rfbInput{kind: rfbPointer, x: 6})
	q.push(rfbInput{kind: rfbPointer, x: 7})

	var xs []int
	for _, in := range q.take() {
		if in.kind == rfbPointer {
			xs = append(xs, in.x)
		} else {
			xs = append(xs, -1)
		}
	}
	if want := []int{2, 4, 5, -1, 7}; len(xs) != len(want) || xs[0] != 2 || xs[1] != 4 || xs[2] != 5 || xs[3] != -1 || xs[4] != 7 {
		t.Errorf("kept %v, want %v", xs, want)
	}
	if len(q.take()) != 0 {
		t.Error("the queue was not emptied")
	}
}

func TestClipboardTextCrossesAsLatin1(t *testing.T) {
	if got := latin1([]byte{'c', 'a', 'f', 0xe9}); got != "café" {
		t.Errorf("read %q", got)
	}
	if got := toLatin1("café €"); !bytes.Equal(got, []byte{'c', 'a', 'f', 0xe9, ' ', '?'}) {
		t.Errorf("wrote % x", got)
	}
}
