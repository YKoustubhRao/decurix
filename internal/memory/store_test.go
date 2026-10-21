package memory

import (
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newStore(t *testing.T) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestStoreMemoryLifecycle(t *testing.T) {
	s := newStore(t)

	entry, err := s.Record("desktop-1", "Lead follow-up", "Check the CRM, draft a reply, and send it from the shared mailbox.", "workflow")
	if err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	if entry.Title != "Lead follow-up" {
		t.Fatalf("expected title to be recorded, got %q", entry.Title)
	}

	memories := s.List("desktop-1")
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory for desktop-1, got %d", len(memories))
	}

	if memories[0].Summary == "" {
		t.Fatal("expected summary to be retained")
	}
}

// A worker's memory is the one thing it learned; losing it to a control-plane
// restart would make every lesson temporary.
func TestMemorySurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlitedb.FileName)

	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Record("desktop-1", "Invoices", "Approve in the admin panel.", ""); err != nil {
		t.Fatal(err)
	}

	db.Close()

	db, err = sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s, err = NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	if got := s.List(""); len(got) != 1 || got[0].Category != "workflow" {
		t.Fatalf("after reopening: %+v", got)
	}
}

func TestStoreLinkSharesInBothDirections(t *testing.T) {
	s := newStore(t)

	if err := s.Link("desktop-a", "desktop-b"); err != nil {
		t.Fatalf("Link returned error: %v", err)
	}

	// Linking again, from either direction, must not duplicate the pair.
	if err := s.Link("desktop-b", "desktop-a"); err != nil {
		t.Fatalf("relinking returned error: %v", err)
	}

	if got := s.LinkedWorkers("desktop-a"); len(got) != 1 || got[0] != "desktop-b" {
		t.Fatalf("expected desktop-a linked to [desktop-b], got %v", got)
	}

	if got := s.LinkedWorkers("desktop-b"); len(got) != 1 || got[0] != "desktop-a" {
		t.Fatalf("expected desktop-b linked to [desktop-a], got %v", got)
	}

	if err := s.Unlink("desktop-a", "desktop-b"); err != nil {
		t.Fatalf("Unlink returned error: %v", err)
	}

	if got := s.LinkedWorkers("desktop-a"); len(got) != 0 {
		t.Fatalf("expected no links after Unlink, got %v", got)
	}
}

func TestStoreLinkRejectsSelfAndEmpty(t *testing.T) {
	s := newStore(t)

	if err := s.Link("desktop-a", "desktop-a"); err == nil {
		t.Fatal("expected an error linking a worker to itself")
	}

	if err := s.Link("desktop-a", ""); err == nil {
		t.Fatal("expected an error linking to an empty desktop ID")
	}
}

func TestStoreRecordDeduplication(t *testing.T) {
	s := newStore(t)

	entry1, err := s.Record("desktop-1", "Deploy Script", "Run ./deploy.sh to push production code.", "workflow")
	if err != nil {
		t.Fatalf("first Record failed: %v", err)
	}

	// Record exact duplicate
	entry2, err := s.Record("desktop-1", "Deploy Script", "Run ./deploy.sh to push production code.", "decision")
	if err != nil {
		t.Fatalf("second Record failed: %v", err)
	}

	if entry1.ID != entry2.ID {
		t.Fatalf("expected duplicate record to reuse ID %q, got %q", entry1.ID, entry2.ID)
	}

	if entry2.Category != "decision" {
		t.Fatalf("expected category to update to %q, got %q", "decision", entry2.Category)
	}

	list := s.List("desktop-1")
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 record after deduplication, got %d", len(list))
	}
}

// Deleting a desktop takes what it learned with it: a desktop made later
// under the same name is another worker, and used to list this one's memory.
func TestDeleteForDesktopRemovesItsEntriesAndLinksOnly(t *testing.T) {
	s := newStore(t)

	first, err := s.Record("eve", "Search Google", "Run it with /skill search-google.", "skill")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Record("eve", "Month end", "Close the books on the last working day.", "workflow")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record("ada", "Invoices", "Invoices go into the purchases sheet.", "workflow"); err != nil {
		t.Fatal(err)
	}

	if err := s.Link("eve", "ada"); err != nil {
		t.Fatal(err)
	}
	if err := s.Link("ada", "bob"); err != nil {
		t.Fatal(err)
	}

	ids, err := s.DeleteForDesktop(" eve ")
	if err != nil {
		t.Fatalf("DeleteForDesktop returned error: %v", err)
	}

	removed := map[string]bool{}
	for _, id := range ids {
		removed[id] = true
	}
	if len(ids) != 2 || !removed[first.ID] || !removed[second.ID] {
		t.Fatalf("expected the two deleted entries' IDs, got %v", ids)
	}

	if got := s.List("eve"); len(got) != 0 {
		t.Fatalf("expected no memory left for the deleted desktop, got %v", got)
	}
	if got := s.List("ada"); len(got) != 1 {
		t.Fatalf("another desktop's memory must stay, got %v", got)
	}

	if got := s.LinkedWorkers("eve"); len(got) != 0 {
		t.Fatalf("expected the deleted desktop's links gone, got %v", got)
	}
	if got := s.LinkedWorkers("ada"); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("expected ada still linked to [bob] only, got %v", got)
	}

	// Nothing to delete, and no desktop named: neither is an error.
	if ids, err := s.DeleteForDesktop("eve"); err != nil || len(ids) != 0 {
		t.Fatalf("expected a second delete to do nothing, got %v, %v", ids, err)
	}
	if ids, err := s.DeleteForDesktop(""); err != nil || len(ids) != 0 {
		t.Fatalf("expected an empty desktop ID to do nothing, got %v, %v", ids, err)
	}
	if got := s.List("ada"); len(got) != 1 {
		t.Fatalf("an empty desktop ID must delete nothing, got %v", got)
	}
}
