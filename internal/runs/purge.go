// purge.go is the run bundles' part of purging a leaked secret by value (internal/purge). A bundle's files are blobs,
// purged in the blob store under the names the manifest keeps, so a bundle still opens every file it lists, at the
// length it lists. Here a manifest row is rewritten only where its own text (a path, a missing part's reason) holds
// the value; its manifest_sha stays the one computed when it was pulled, as a blob keeps its name. It is the one
// change ever made to a bundle row: a secret is not a label.

package runs

import (
	"encoding/json"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// purgePage is how many manifests one read of a purge holds.
const purgePage = 200

// PurgeValue rewrites every bundle row whose files, history or missing list hold any of forms, each replaced by mark,
// and returns how many bundles held the value: those rewritten, and those listing a blob in purgedBlobs (by name). It
// reads a page of rows at a time and writes only once that page's rows are closed, under the store's lock so no pull
// merges into a row between the read and the write.
func (s *Store) PurgeValue(forms []string, mark string, purgedBlobs map[string]bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type row struct{ exec, files, history, missing string }
	var n int64
	last := ""
	for {
		page, err := sqlitedb.QueryAll(s.db, func(sc sqlitedb.Scanner) (row, error) {
			var r row
			return r, sc.Scan(&r.exec, &r.files, &r.history, &r.missing)
		}, `SELECT exec_id, files, history, missing FROM run_bundles WHERE exec_id > ? ORDER BY exec_id LIMIT ?`,
			last, purgePage)
		if err != nil {
			return n, err
		}
		if len(page) == 0 {
			return n, nil
		}
		for _, r := range page {
			last = r.exec
			files, c1 := sqlitedb.PurgeText(r.files, forms, mark)
			history, c2 := sqlitedb.PurgeText(r.history, forms, mark)
			missing, c3 := sqlitedb.PurgeText(r.missing, forms, mark)
			if c1 || c2 || c3 {
				if _, err := s.db.Exec(`UPDATE run_bundles SET files = ?, history = ?, missing = ? WHERE exec_id = ?`,
					files, history, missing, r.exec); err != nil {
					return n, err
				}
				n++
				continue
			}
			if listsAny(r.files, purgedBlobs) || listsAny(r.history, purgedBlobs) {
				n++
			}
		}
	}
}

// listsAny reports whether a manifest's files (JSON) name one of purged.
func listsAny(filesJSON string, purged map[string]bool) bool {
	if len(purged) == 0 {
		return false
	}
	var files []File
	_ = json.Unmarshal([]byte(filesJSON), &files)
	for _, f := range files {
		if purged[f.SHA256] {
			return true
		}
	}
	return false
}
