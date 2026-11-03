package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/atomizer"
	"thebot/internal/memory"
	"thebot/internal/rag"
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

// A chat request's body is bounded (review T-23): an oversized one is refused before it is read whole.
func TestAnOversizedChatBodyIsRefused(t *testing.T) {
	s := newTestServer(t)
	body := `{"prompt": "` + strings.Repeat("x", maxChatBody+10) + `"}`
	if rec := serve(s, http.MethodPost, "/api/chat", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// A store error reaches the log, never the client (review T-23, CLAUDE.md: jsonError never echoes err.Error()).
func TestAnUnknownSkillIsAnsweredWithoutTheError(t *testing.T) {
	s := newTestServer(t)
	lib, err := memory.NewSkillLibrary(s.ws.Root(), nil, "m")
	if err != nil {
		t.Fatal(err)
	}
	s.sched.SkillLib = lib
	rec := serve(s, http.MethodPost, "/api/skills/no-such-skill/confirm", "")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), s.ws.Root()) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

// A store a newer soul wrote is started without, and said so; any other store error still stops soul (N-W3-7).
func TestAStoreANewerSoulWroteIsStartedWithout(t *testing.T) {
	root := t.TempDir()
	r, err := rag.New(root, fakeEmbedder{}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	// Move the store one step past what this soul knows, as a newer soul would have left it.
	db, err := sql.Open("sqlite", filepath.Join(root, ".rag", "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(rag.Migrate)+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = rag.New(root, fakeEmbedder{}, "")
	var off []string
	if err == nil || !storeOff("the Data RAG", err, &off) || len(off) != 1 {
		t.Fatalf("a newer store: err %v, off %v", err, off)
	}
	if storeOff("x", errors.New("disk full"), &off) {
		t.Error("an ordinary error was started through")
	}
	s := newTestServer(t)
	s.storesOff = off
	if rec := serve(s, http.MethodGet, "/api/health", ""); !strings.Contains(rec.Body.String(), "the Data RAG") {
		t.Errorf("health does not say the store is off: %s", rec.Body.String())
	}
}
