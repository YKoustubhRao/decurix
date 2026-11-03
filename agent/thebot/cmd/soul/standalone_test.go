package main

import (
	"context"
	"net/http"
	"testing"

	"thebot/internal/atomizer"
)

// fakeEmbedder stands in for a real embedder: skillLibraryFor must not need one to say no.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(ctx context.Context, model, text string) ([]float32, error) {
	return []float32{1}, nil
}

// Inside Decurix (no THEBOT_STANDALONE=1) the acquired-skill library is not built, even with an embedder and the gate
// present; standalone it is (owner decision O-8, review T-07).
func TestTheSkillLibraryIsOnlyBuiltStandalone(t *testing.T) {
	t.Setenv("DECURIX_GATE_REQUIRED", "1")
	t.Setenv("THEBOT_STANDALONE", "")
	if standaloneMode() {
		t.Fatal("standalone without THEBOT_STANDALONE=1")
	}
	lib, err := skillLibraryFor(standaloneMode(), t.TempDir(), fakeEmbedder{}, "m")
	if err != nil || lib != nil {
		t.Fatalf("embedded: skill library = %v, %v; want none", lib, err)
	}
	t.Setenv("THEBOT_STANDALONE", "1")
	lib, err = skillLibraryFor(standaloneMode(), t.TempDir(), fakeEmbedder{}, "m")
	if err != nil || lib == nil {
		t.Fatalf("standalone: skill library = %v, %v; want one", lib, err)
	}
}

// With no skill library, there is nothing to confirm: the confirm route is 404 (review T-07).
func TestTheConfirmRouteIsGoneWithoutASkillLibrary(t *testing.T) {
	s := newTestServer(t) // SkillLib is nil, as inside Decurix
	if rec := serve(s, http.MethodPost, "/api/skills/any/confirm", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("confirm status = %d, want 404", rec.Code)
	}
}

// With the coding harness off, a request to build software is the operator's, never a skill that is not there.
func TestABuildRequestGoesToTheOperatorWhenCodingIsOff(t *testing.T) {
	was := atomizer.CodingAvailable
	defer func() { atomizer.CodingAvailable = was }()
	atomizer.CodingAvailable = false
	const p = "build a FastAPI service with a SQLite store and tests"
	if got := atomizer.Classify(p); got != "operator" {
		t.Errorf("Classify = %q, want operator", got)
	}
	if got := atomizer.Triage(p); got.TaskType == "coding" {
		t.Errorf("Triage = %+v, want no coding route", got)
	}
	atomizer.CodingAvailable = true
	if got := atomizer.Classify(p); got != "coding" {
		t.Errorf("standalone Classify = %q, want coding", got)
	}
}
