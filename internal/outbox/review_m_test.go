package outbox

import (
	"errors"
	"testing"
)

// One desktop cannot leave more than MaxOpenDrafts emails waiting; sending or
// discarding one frees a place, and other desktops are not affected
// (review 2026-11-03, B-28).
func TestADesktopsWaitingDraftsAreCapped(t *testing.T) {
	s := newStore(t)
	var last Draft
	for i := 0; i < MaxOpenDrafts; i++ {
		d, err := s.Add(Draft{DesktopID: "ops", To: []string{"a@x.com"}, Body: "b"})
		if err != nil {
			t.Fatalf("draft %d: %v", i, err)
		}
		last = d
	}

	if _, err := s.Add(Draft{DesktopID: "ops", To: []string{"a@x.com"}, Body: "b"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("draft past the cap: %v", err)
	}
	if _, err := s.Add(Draft{DesktopID: "other", To: []string{"a@x.com"}, Body: "b"}); err != nil {
		t.Fatalf("another desktop was refused: %v", err)
	}

	if err := s.Discard(last.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Draft{DesktopID: "ops", To: []string{"a@x.com"}, Body: "b"}); err != nil {
		t.Fatalf("a freed place was not usable: %v", err)
	}
}

func TestADesktopsWaitingFilesAreCapped(t *testing.T) {
	s := newStore(t)
	defer func(v int) { openFileBytes = v }(openFileBytes)
	openFileBytes = 4 << 20
	file := File{Name: "a.pdf", Data: make([]byte, 1<<20)}
	n := 4
	for i := 0; i < n; i++ {
		if _, err := s.Add(Draft{DesktopID: "ops", To: []string{"a@x.com"}, Files: []File{file}}); err != nil {
			t.Fatalf("draft %d: %v", i, err)
		}
	}
	if _, err := s.Add(Draft{DesktopID: "ops", To: []string{"a@x.com"}, Files: []File{file}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("files past the cap: %v", err)
	}
}
