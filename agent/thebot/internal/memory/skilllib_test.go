package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A proposal is a real, honest file on disk in pending/ - never matched
// against anything, never trusted, until a human confirms it.
func TestProposeWritesARealPendingFileAndNothingElse(t *testing.T) {
	root := t.TempDir()
	lib, err := NewSkillLibrary(root, nil, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}

	sk, err := lib.Propose("installing python packages when pip is externally managed",
		[]string{"run the command: python3 -m venv v", "run the command: v/bin/pip install X"}, "decomposed-recovery")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if sk.Slug == "" {
		t.Fatal("Propose() returned an empty slug")
	}

	pendingPath := filepath.Join(root, ".skills", "pending", sk.Slug+".md")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("no real pending file was written: %v", err)
	}
	raw, _ := os.ReadFile(pendingPath)
	if !strings.Contains(string(raw), "trigger:") || !strings.Contains(string(raw), "python3 -m venv") {
		t.Errorf("pending file content = %q, missing expected fields", raw)
	}

	// Nothing here is matchable yet - it hasn't been confirmed.
	confirmed, _ := lib.ListConfirmed()
	if len(confirmed) != 0 {
		t.Errorf("ListConfirmed() = %+v, want empty before confirmation", confirmed)
	}
	pending, err := lib.ListPending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending() = %+v, err=%v, want exactly 1", pending, err)
	}
}

// Confirm is the ONLY thing that moves a skill from "proposed" to
// "trusted" - the real gate a human's approval sits behind.
func TestConfirmMovesToConfirmedAndClearsThePending(t *testing.T) {
	provider := realEmbedder(t)
	root := t.TempDir()
	lib, err := NewSkillLibrary(root, provider, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}

	sk, err := lib.Propose("a test trigger", []string{"do a real thing"}, "research")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}

	confirmed, err := lib.Confirm(context.Background(), sk.Slug)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if confirmed.Trigger != sk.Trigger {
		t.Errorf("Confirm() trigger = %q, want %q", confirmed.Trigger, sk.Trigger)
	}

	if _, err := os.Stat(filepath.Join(root, ".skills", "pending", sk.Slug+".md")); !os.IsNotExist(err) {
		t.Error("the pending copy was not removed after confirmation")
	}
	list, err := lib.ListConfirmed()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListConfirmed() = %+v, err=%v, want exactly 1 after confirming", list, err)
	}
}

// Reject discards a proposal permanently - it must never become
// matchable, and must never leave a confirmed copy behind.
func TestRejectDiscardsThePendingProposal(t *testing.T) {
	root := t.TempDir()
	lib, err := NewSkillLibrary(root, nil, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	sk, err := lib.Propose("a trigger to reject", []string{"do a thing"}, "research")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if err := lib.Reject(sk.Slug); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	pending, _ := lib.ListPending()
	if len(pending) != 0 {
		t.Errorf("ListPending() = %+v, want empty after rejection", pending)
	}
	confirmed, _ := lib.ListConfirmed()
	if len(confirmed) != 0 {
		t.Errorf("ListConfirmed() = %+v, want empty - a rejected skill must never appear as confirmed", confirmed)
	}
}

// The real point of the whole library: a confirmed skill is found for a
// SEMANTICALLY similar future request, even when the wording differs.
func TestMatchFindsASemanticallySimilarConfirmedSkill(t *testing.T) {
	provider := realEmbedder(t)
	root := t.TempDir()
	lib, err := NewSkillLibrary(root, provider, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}

	sk, err := lib.Propose(
		"installing python packages fails because pip reports the environment is externally managed",
		[]string{"run the command: python3 -m venv v", "run the command: v/bin/pip install <packages>"},
		"decomposed-recovery")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if _, err := lib.Confirm(context.Background(), sk.Slug); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}

	matches, err := lib.Match(context.Background(), "pip install requests fails with externally-managed-environment", 3)
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("Match() = %+v, want exactly 1 real match", matches)
	}
	if matches[0].Slug != sk.Slug {
		t.Errorf("Match()[0].Slug = %q, want %q", matches[0].Slug, sk.Slug)
	}
	if len(matches[0].Steps) != 2 {
		t.Errorf("Match()[0].Steps = %+v, want the real stored recipe preserved", matches[0].Steps)
	}
}

// An unrelated request must not match - a weak, irrelevant "skill" reused
// as if proven would be worse than finding nothing at all.
func TestMatchReturnsNothingForAnUnrelatedRequest(t *testing.T) {
	provider := realEmbedder(t)
	root := t.TempDir()
	lib, err := NewSkillLibrary(root, provider, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	sk, err := lib.Propose("installing python packages when pip is externally managed",
		[]string{"run the command: python3 -m venv v"}, "research")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if _, err := lib.Confirm(context.Background(), sk.Slug); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}

	matches, err := lib.Match(context.Background(), "what is the capital of France?", 3)
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("Match() = %+v, want no matches for a completely unrelated request", matches)
	}
}

// A hand-edited or otherwise malformed skill file must be skipped, never
// crash the whole library.
func TestListSkillFilesSkipsMalformedFilesWithoutCrashing(t *testing.T) {
	root := t.TempDir()
	lib, err := NewSkillLibrary(root, nil, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	confirmedDir := filepath.Join(root, ".skills", "confirmed")
	if err := os.WriteFile(filepath.Join(confirmedDir, "broken.md"), []byte("not a real skill file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Propose("a real one", []string{"do a thing"}, "research"); err != nil {
		t.Fatal(err)
	}

	list, err := lib.ListConfirmed()
	if err != nil {
		t.Fatalf("ListConfirmed() error = %v, want it to skip the broken file rather than fail", err)
	}
	if len(list) != 0 {
		t.Errorf("ListConfirmed() = %+v, want empty (the malformed file skipped, the real one still pending)", list)
	}
}
