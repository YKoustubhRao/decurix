package claudeauth

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sealed"
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

// A store with a key keeps the login sealed: the database holds no token, and Get gives it back as saved.
func TestStoreSealsTheLoginWithAKey(t *testing.T) {
	s := newMemoryStore(t)
	key := testKey(t)
	if err := s.UseKey(key); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(`{"claudeAiOauth":{"refreshToken":"rt-secret"}}`, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if raw := storedCredentials(t, s); !sealed.IsSealed(raw) || strings.Contains(raw, "rt-secret") {
		t.Fatalf("stored = %q, want it sealed", raw)
	}
	session, ok := s.Get()
	if !ok || session.CredentialsJSON != `{"claudeAiOauth":{"refreshToken":"rt-secret"}}` || session.Email != "me@example.com" {
		t.Fatalf("Get = %+v, %v", session, ok)
	}
}

// A login an older build saved unsealed is sealed in place when the key is given, and still reads the same.
func TestStoreSealsAnOldUnsealedLogin(t *testing.T) {
	s := newMemoryStore(t)
	if err := s.Save(`{"token":"old-plain"}`, "a@example.com"); err != nil { // no key yet: as an older build kept it
		t.Fatal(err)
	}
	if raw := storedCredentials(t, s); raw != `{"token":"old-plain"}` {
		t.Fatalf("stored = %q", raw)
	}
	if err := s.UseKey(testKey(t)); err != nil {
		t.Fatal(err)
	}
	if raw := storedCredentials(t, s); !sealed.IsSealed(raw) || strings.Contains(raw, "old-plain") {
		t.Fatalf("after UseKey stored = %q, want it sealed", raw)
	}
	if session, ok := s.Get(); !ok || session.CredentialsJSON != `{"token":"old-plain"}` {
		t.Fatalf("Get = %+v, %v", session, ok)
	}

	// An unsealed row written behind the store's back (a restored copy) is sealed on its first read.
	if _, err := s.db.Exec(`UPDATE claude_session SET credentials_json = '{"token":"restored"}' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if session, ok := s.Get(); !ok || session.CredentialsJSON != `{"token":"restored"}` {
		t.Fatalf("Get = %+v, %v", session, ok)
	}
	if raw := storedCredentials(t, s); !sealed.IsSealed(raw) {
		t.Fatalf("after Get stored = %q, want it sealed", raw)
	}
}

// A sealed login the key cannot open (the key file was replaced) reads as no login: logging in again fixes it.
func TestStoreWithTheWrongKeyHasNoLogin(t *testing.T) {
	s := newMemoryStore(t)
	if err := s.UseKey(testKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(`{"token":"x"}`, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	s.key = testKey(t)
	if _, ok := s.Get(); ok {
		t.Fatal("a login sealed under another key was read")
	}
}

// testKey is a fresh key from sealed.LoadKey in a temporary folder.
func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := sealed.LoadKey(t.TempDir(), KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// storedCredentials is the credentials column as the database holds it.
func storedCredentials(t *testing.T, s *Store) string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(`SELECT credentials_json FROM claude_session WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
