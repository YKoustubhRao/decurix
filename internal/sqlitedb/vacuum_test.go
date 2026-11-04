package sqlitedb

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database made before incremental vacuum was asked for (auto_vacuum NONE) gets it once, by one VACUUM, and keeps
// its rows; freed pages then go back to the filesystem; a database that has it is left alone (resource plan B2).
func TestAnOldDatabaseGetsIncrementalVacuumOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	raw, err := open((&url.URL{Scheme: "file", Path: uriPath}).String(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if _, err := raw.Exec(`INSERT INTO t (body) VALUES (?)`, strings.Repeat("x", 2000)); err != nil {
			t.Fatal(err)
		}
	}
	var mode int
	_ = raw.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode)
	if mode != 0 {
		t.Fatalf("the old database has auto_vacuum %d, want 0 to start from", mode)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum after Open: %d %v, want 2 (INCREMENTAL)", mode, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&rows); err != nil || rows != 400 {
		t.Fatalf("rows after the VACUUM: %d %v", rows, err)
	}

	// Freed pages go back: delete most rows, vacuum incrementally, and the file's page count falls.
	pages := func() int {
		var n int
		_ = db.QueryRow(`PRAGMA page_count`).Scan(&n)
		return n
	}
	before := pages()
	if _, err := db.Exec(`DELETE FROM t WHERE id > 20`); err != nil {
		t.Fatal(err)
	}
	if err := IncrementalVacuum(db, 100000); err != nil {
		t.Fatal(err)
	}
	if after := pages(); after >= before {
		t.Fatalf("page count %d -> %d after deleting and vacuuming", before, after)
	}

	// A database that has it is not vacuumed again.
	if did, err := ensureIncrementalVacuum(db, path); did || err != nil {
		t.Fatalf("a database with incremental vacuum was vacuumed again: %v %v", did, err)
	}
}

// KeepVacuumed stops when its context ends.
func TestKeepVacuumedStopsWithItsContext(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { KeepVacuumed(ctx, db, time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("KeepVacuumed did not stop")
	}
}
