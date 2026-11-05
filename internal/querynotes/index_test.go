package querynotes

import (
	"strings"
	"testing"
)

// The open notes and the resolved history are read by index, not by a scan of the table (resource plan P1 #6).
func TestTheQueriesRailReadsByIndex(t *testing.T) {
	s := newTestStore(t)
	plan := func(where, order string) string {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + listQuery(where, order))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, " | ")
	}
	// The store's own clauses, so a query that drifts is checked as it now is.
	for _, q := range [][2]string{{openWhere, openOrder}, {resolvedWhere, resolvedOrder}} {
		if p := plan(q[0], q[1]); !strings.Contains(p, "query_notes_resolved_created") || strings.Contains(p, "TEMP B-TREE") {
			t.Errorf("%s ORDER BY %s: plan %q", q[0], q[1], p)
		}
	}
}
