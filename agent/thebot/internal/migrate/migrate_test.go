package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Steps run once each, in order; a database made before (its tables already there, version 0) is left as it was; a
// later step appended runs on the next open.
func TestMigrateRunsEachStepOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (a TEXT); INSERT INTO t VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	steps := []string{`CREATE TABLE IF NOT EXISTS t (a TEXT)`}
	for i := 0; i < 2; i++ {
		if err := Migrate(db, steps); err != nil {
			t.Fatal(err)
		}
	}
	steps = append(steps, `ALTER TABLE t ADD COLUMN b TEXT`)
	if err := Migrate(db, steps); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db, steps); err != nil {
		t.Fatalf("a step ran twice: %v", err)
	}
	var a string
	var v int
	if err := db.QueryRow(`SELECT a FROM t`).Scan(&a); err != nil || a != "kept" {
		t.Fatalf("existing row: %q %v", a, err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("version %d %v", v, err)
	}
	if err := Migrate(db, steps[:1]); err == nil {
		t.Error("an older soul opened a newer database without a word")
	}
}
