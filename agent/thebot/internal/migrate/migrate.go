// Package migrate keeps soul's SQLite stores' schemas as an append-only list of steps (CLAUDE.md: a schema change is a
// new appended step, never an edit; review T-28).
package migrate

import (
	"database/sql"
	"fmt"
)

// Migrate runs, in order, each step of steps the database has not had yet (its PRAGMA user_version counts those it
// has), each in its own transaction with the version moved on in the same one. Steps are only ever appended: the first
// is a store's schema as it stood, written with IF NOT EXISTS, so a database made before this package is unchanged.
func Migrate(db *sql.DB, steps []string) error {
	var have int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("migrate: read version: %w", err)
	}
	if have > len(steps) {
		return fmt.Errorf("migrate: the database is at step %d, newer than this soul's %d", have, len(steps))
	}
	for i := have; i < len(steps); i++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migrate: begin step %d: %w", i+1, err)
		}
		if _, err := tx.Exec(steps[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: step %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: record step %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate: commit step %d: %w", i+1, err)
		}
	}
	return nil
}
