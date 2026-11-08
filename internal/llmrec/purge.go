// purge.go is the call log's part of purging a leaked secret by value (internal/purge): the forms a value takes in a
// stored message, and rewriting the rows whose own columns hold it. The bodies are blobs, purged in the blob store
// under the names the rows keep, so a row still joins its messages and its prompt prefix after a purge. This is the
// one change ever made to a stored row (Store: "never updates or deletes a row"): a secret is not a label.

package llmrec

import (
	"encoding/json"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// purgePage is how many rows one read of a purge holds.
const purgePage = 500

// SecretForms is every form values can take in a stored message, longest first: as written, and each JSON-string
// escaping a writer may have used (the forms the recorder withholds).
func SecretForms(values ...string) []string { return secretForms(values) }

// PurgeValue rewrites every llm_calls row whose params or problems hold any of forms, each replaced by mark, and
// returns how many rows held the value: those rewritten, and those whose messages, answer or tool list is one of the
// blobs purged (by name). It reads the rows a page at a time and writes only once that page's rows are closed.
func (s *Store) PurgeValue(forms []string, mark string, purgedBlobs map[string]bool) (int64, error) {
	type row struct {
		rowid                                int64
		messages, response, params, problems string
	}
	var n int64
	last := int64(0)
	for {
		page, err := sqlitedb.QueryAll(s.db, func(sc sqlitedb.Scanner) (row, error) {
			var r row
			return r, sc.Scan(&r.rowid, &r.messages, &r.response, &r.params, &r.problems)
		}, `SELECT rowid, messages, response_sha, params, problems FROM llm_calls WHERE rowid > ? ORDER BY rowid LIMIT ?`,
			last, purgePage)
		if err != nil {
			return n, err
		}
		if len(page) == 0 {
			return n, nil
		}
		for _, r := range page {
			last = r.rowid
			params, p1 := sqlitedb.PurgeText(r.params, forms, mark)
			problems, p2 := sqlitedb.PurgeText(r.problems, forms, mark)
			if p1 || p2 {
				if _, err := s.db.Exec(`UPDATE llm_calls SET params = ?, problems = ? WHERE rowid = ?`, params, problems,
					r.rowid); err != nil {
					return n, err
				}
				n++
				continue
			}
			if refersTo(r.messages, r.response, r.params, purgedBlobs) {
				n++
			}
		}
	}
}

// refersTo reports whether a row's messages, answer or tool list names one of purged.
func refersTo(messages, response, params string, purged map[string]bool) bool {
	if len(purged) == 0 {
		return false
	}
	if purged[response] {
		return true
	}
	var shas []string
	_ = json.Unmarshal([]byte(messages), &shas)
	for _, sha := range shas {
		if purged[sha] {
			return true
		}
	}
	var p struct {
		Tools struct {
			SHA256 string `json:"sha256"`
		} `json:"tools"`
	}
	_ = json.Unmarshal([]byte(params), &p)
	return p.Tools.SHA256 != "" && purged[p.Tools.SHA256]
}
