package sqlitedb

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenEscapesFileName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data #1%25.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE kept (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at requested path: %v", err)
	}
}

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

func TestIncrementalVacuum(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := IncrementalVacuum(db, 10); err != nil {
		t.Fatalf("incremental vacuum failed: %v", err)
	}
}

// A transaction that reads, then writes after another connection committed,
// must wait its turn rather than fail: a deferred BEGIN answered that write
// with SQLITE_BUSY_SNAPSHOT (517, "database is locked"), which busy_timeout
// does not retry - live, a finished lesson's outcome was lost that way and
// its task stayed "running" for good.
func TestAReadThenWriteTransactionIsNotLostToAConcurrentCommit(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "tx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE n (id INTEGER PRIMARY KEY, v INTEGER); INSERT INTO n VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := tx.QueryRow(`SELECT v FROM n WHERE id = 1`).Scan(&v); err != nil {
		t.Fatal(err)
	}

	// Another connection writes while the transaction is open.
	done := make(chan error, 1)
	go func() {
		_, err := db.Exec(`UPDATE n SET v = v + 10 WHERE id = 1`)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)

	if _, err := tx.Exec(`UPDATE n SET v = ? WHERE id = 1`, v+1); err != nil {
		t.Fatalf("the transaction's own write failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the other write failed: %v", err)
	}

	if err := db.QueryRow(`SELECT v FROM n WHERE id = 1`).Scan(&v); err != nil || v != 11 {
		t.Fatalf("v = %d, %v; want 11 (both writes kept, one after the other)", v, err)
	}
}
