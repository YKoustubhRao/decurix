package memory

import (
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

func TestStoreMemoryLifecycle(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

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
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

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
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Link("desktop-a", "desktop-a"); err == nil {
		t.Fatal("expected an error linking a worker to itself")
	}

	if err := s.Link("desktop-a", ""); err == nil {
		t.Fatal("expected an error linking to an empty desktop ID")
	}
}
