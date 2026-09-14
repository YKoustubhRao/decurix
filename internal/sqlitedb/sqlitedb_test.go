package sqlitedb

import (
	"path/filepath"
	"testing"
	"time"
)

// A step already applied is skipped on the next start, and one appended
// later is applied on top - which is what lets a component grow its schema
// without a restart ever re-running CREATE TABLE against real data.
func TestMigrateAppliesOnlyNewSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	steps := []string{`CREATE TABLE things (id TEXT PRIMARY KEY)`}

	if err := Migrate(db, "things", steps); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO things (id) VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}

	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	steps = append(steps, `ALTER TABLE things ADD COLUMN note TEXT NOT NULL DEFAULT ''`)

	if err := Migrate(db, "things", steps); err != nil {
		t.Fatalf("re-running migrations after a restart: %v", err)
	}

	var id, note string

	if err := db.QueryRow(`SELECT id, note FROM things`).Scan(&id, &note); err != nil {
		t.Fatalf("row written before the restart is gone: %v", err)
	}

	if id != "kept" {
		t.Fatalf("got %q", id)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	now := time.Now().UTC()

	if got := FromNS(ToNS(now)); !got.Equal(now) {
		t.Fatalf("got %v, want %v", got, now)
	}

	if !FromNS(ToNS(time.Time{})).IsZero() {
		t.Fatal("the zero time must survive a round trip")
	}
}
