package memory

import (
	"strings"
	"testing"
)

// The newest entries of every desktop are read by index, not by scanning and sorting the table (resource plan P1 #6).
func TestEveryDesktopsNewestEntriesAreReadByIndex(t *testing.T) {
	s := newStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT id, desktop_id, title, summary, category, created_ns FROM worker_memory ORDER BY created_ns DESC LIMIT 2000`)
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
