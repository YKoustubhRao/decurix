package main

import "testing"

func TestManagerMemoryLifecycle(t *testing.T) {
	m := &Manager{
		client:       nil,
		helpNotes:    make(map[string]HelpNote),
		workerMemory: make(map[string][]WorkerMemoryEntry),
	}

	entry, err := m.recordMemory("desktop-1", "Lead follow-up", "Check the CRM, draft a reply, and send it from the shared mailbox.", "workflow")
	if err != nil {
		t.Fatalf("recordMemory returned error: %v", err)
	}

	if entry.Title != "Lead follow-up" {
		t.Fatalf("expected title to be recorded, got %q", entry.Title)
	}

	memories := m.listMemories("desktop-1")
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory for desktop-1, got %d", len(memories))
	}

	if memories[0].Summary == "" {
		t.Fatal("expected summary to be retained")
	}
}
