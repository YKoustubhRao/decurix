package claudeauth

import (
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// newMemoryStore is a Store on a fresh in-memory database.
func newMemoryStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestStoreGetEmptyByDefault(t *testing.T) {
	s := newMemoryStore(t)

	if _, ok := s.Get(); ok {
		t.Fatal("expected no session before anyone has logged in")
	}
}

func TestStoreSaveOverwritesTheSameRow(t *testing.T) {
	s := newMemoryStore(t)

	if err := s.Save(`{"token":"first"}`, "a@example.com"); err != nil {
		t.Fatal(err)
	}

	if err := s.Save(`{"token":"second"}`, "b@example.com"); err != nil {
		t.Fatal(err)
	}

	session, ok := s.Get()
	if !ok {
		t.Fatal("expected a stored session")
	}

	if session.CredentialsJSON != `{"token":"second"}` || session.Email != "b@example.com" {
		t.Fatalf("expected the second login to replace the first, got %+v", session)
	}
}

// The shared session is what every claude-harness desktop gets seeded from,
// so it needs to survive a control-plane restart the same way task records
// and chats do - a rebuild wiping it is the only way it should ever go away.
func TestStoreSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlitedb.FileName)

	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Save(`{"token":"abc"}`, "worker@example.com"); err != nil {
		t.Fatal(err)
	}

	db.Close()

	db, err = sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s, err = NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	session, ok := s.Get()
	if !ok || session.CredentialsJSON != `{"token":"abc"}` || session.Email != "worker@example.com" {
		t.Fatalf("after reopening: ok=%v session=%+v", ok, session)
	}
}
