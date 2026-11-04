package memory

import (
	"strings"
	"testing"
)

// The newest entries of every desktop are read by index, not by scanning and sorting the table (resource plan P1 #6).
func TestEveryDesktopsNewestEntriesAreReadByIndex(t *testing.T) {
	s := newStore(t)
	query, args := listQuery("") // the store's own query
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	p := strings.Join(plan, " | ")
	if !strings.Contains(p, "worker_memory_created") || strings.Contains(p, "TEMP B-TREE") {
		t.Fatalf("plan %q", p)
	}
}
