package memory

import "testing"

func TestStoreMemoryLifecycle(t *testing.T) {
	s := NewStore()

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
