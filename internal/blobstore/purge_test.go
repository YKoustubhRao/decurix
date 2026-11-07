// purge_test.go checks the in-place purge of a value from blobs: every copy found wherever it falls against the read
// chunks, the length and the name kept, and the note of the new bytes' hash.

package blobstore

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// Every copy is overwritten by a tombstone of its own length, wherever it falls against the 64 KiB reads, the longest
// form winning where two start together; bytes around it are kept.
func TestReplaceStreamFindsCopiesAcrossChunks(t *testing.T) {
	forms := [][]byte{[]byte(`se\"cret99`), []byte(`se"cret99`)}
	for _, at := range []int{0, 1, 64<<10 - 9, 64<<10 - 4, 64 << 10, 3*64<<10 - 1} {
		in := append(bytes.Repeat([]byte("a"), at), []byte(`se\"cret99|se"cret99`)...)
		in = append(in, bytes.Repeat([]byte("b"), 70<<10)...)
		var out bytes.Buffer
		n, err := replaceStream(bytes.NewReader(in), &out, forms, "[purged]", false)
		if err != nil || n != 2 {
			t.Fatalf("at %d: %d found, %v", at, n, err)
		}
		if out.Len() != len(in) {
			t.Fatalf("at %d: length %d, want %d", at, out.Len(), len(in))
		}
		want := strings.Repeat("a", at) + "[purged]__|[purged]_" + strings.Repeat("b", 70<<10)
		if out.String() != want {
			t.Fatalf("at %d: the rewrite is wrong around the copy: %q", at, out.String()[max(0, at-2):at+24])
		}
	}
}

// The tombstone is the value's length, the mark padded or cut.
func TestTombstoneKeepsTheLength(t *testing.T) {
	for n, want := range map[int]string{6: "[purg]", 7: "[purge]", 8: "[purged]", 11: "[purged]___"} {
		if got := string(Tombstone("[purged]", n)); got != want {
			t.Errorf("Tombstone(%d) = %q, want %q", n, got, want)
		}
	}
}

// A purged blob keeps its name and length, opens with the mark in place of the value, is still Has, and its note
// holds the hash of its new bytes; a blob without the value is not touched, and storing the old bytes again keeps
// the purged ones.
func TestPurgeFormsRewritesInPlaceAndNotesTheNewHash(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"content":"the key is hunter2hunter2, keep it"}`)
	sha, err := s.PutBytes(old)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.PutBytes([]byte("nothing to see"))
	var seen []string
	failed, err := s.PurgeForms([]string{"hunter2hunter2"}, "[purged]", "j_0123456789abcdef", func(x string) { seen = append(seen, x) })
	if err != nil || failed != 0 || len(seen) != 1 || seen[0] != sha {
		t.Fatalf("PurgeForms: %v %d %v", err, failed, seen)
	}
	rc, err := s.Open(sha)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != `{"content":"the key is [purged]______, keep it"}` || len(b) != len(old) {
		t.Fatalf("after the purge: %s", b)
	}
	if !s.Has(sha) {
		t.Error("the purged blob is no longer Has")
	}
	note, ok := s.Purged(sha)
	if !ok || note.SHA256 != sha || note.ContentSHA256 != SHA256(b) || note.Bytes != int64(len(b)) || note.Judgement != "j_0123456789abcdef" {
		t.Errorf("note = %+v %v", note, ok)
	}
	if _, ok := s.Purged(other); ok {
		t.Error("an untouched blob has a purge note")
	}
	if again, _ := s.PutBytes(old); again != sha {
		t.Fatal("the old bytes are named differently")
	}
	rc, _ = s.Open(sha)
	b2, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, b2) {
		t.Error("storing the old bytes again brought the value back")
	}
}
