package outbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestADraftIsSentOnceAndKeptOnRecord(t *testing.T) {
	s := newStore(t)

	d, err := s.Add(Draft{DesktopID: "ops", To: []string{"a@x.com; b@x.com"}, Subject: " Statement ", Body: "Hi",
		Files: []File{{Name: "s.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}}})
	if err != nil {
		t.Fatal(err)
	}

	if len(d.To) != 2 || d.Subject != "Statement" || d.Status != StatusDraft {
		t.Fatalf("draft = %+v", d)
	}

	got, err := s.Claim(d.ID)
	if err != nil || len(got.Files) != 1 || string(got.Files[0].Data) != "%PDF" {
		t.Fatalf("Claim = %+v, %v", got, err)
	}

	if _, err := s.Claim(d.ID); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("a second claim while sending = %v, want ErrNotDraft", err)
	}

	if err := s.Finish(d.ID, nil); err != nil {
		t.Fatal(err)
	}

	list, err := s.List(10)
	if err != nil || len(list) != 1 || list[0].Status != StatusSent || list[0].Files[0].Data != nil || list[0].Files[0].Size != 4 {
		t.Fatalf("List = %+v, %v", list, err)
	}

	if err := s.Discard(d.ID); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("discarding a sent email = %v, want ErrNotDraft", err)
	}
}

func TestAFailedSendCanBeTriedAgain(t *testing.T) {
	s := newStore(t)
	d, _ := s.Add(Draft{To: []string{"a@x.com"}, Subject: "x"})

	_, _ = s.Claim(d.ID)
	_ = s.Finish(d.ID, errors.New("refused"))

	if _, err := s.Claim(d.ID); err != nil {
		t.Fatalf("retrying a failed send = %v", err)
	}
}

func TestAnInterruptedSendIsNeverResentOnItsOwn(t *testing.T) {
	s := newStore(t)
	d, _ := s.Add(Draft{To: []string{"a@x.com"}, Subject: "x"})
	_, _ = s.Claim(d.ID)

	if err := s.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(d.ID)
	if got.Status != StatusFailed || got.Note == "" {
		t.Fatalf("after recovery: %+v", got)
	}
}

func TestADraftNeedsARecipient(t *testing.T) {
	if _, err := newStore(t).Add(Draft{Subject: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestADraftWithNoCcListsNoneRatherThanNull(t *testing.T) {
	s := newStore(t)

	if _, err := s.Add(Draft{DesktopID: "ops", To: []string{"a@x.com"}, Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	list, err := s.List(10)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}

	out, _ := json.Marshal(list[0])
	if !strings.Contains(string(out), `"cc":[]`) {
		t.Fatalf("draft JSON = %s", out)
	}
}
