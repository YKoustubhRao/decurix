// Tests for the speculative-decoding gate: on only when allowed and proven
// on checked cases, and a twin to measure it with.
package engine

import (
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/evals"
)

func specEngine(t *testing.T, speculative string, scores map[string]evals.Score) *Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evals.json")
	r := &evals.Results{}
	for name, s := range scores {
		s.At = time.Now()
		s.Decided, s.DecidedCorrect = s.N, s.Correct
		r.Set(name, "math", s)
	}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	u := &unit{k: "m|src", model: "m", runtime: catalog.RuntimeLlamaCpp, roles: []string{catalog.RoleBrain},
		llama: &LlamaProc{Model: "m.gguf", LogPath: "/logs/llama-m.log"}, rec: "r1"}
	return &Engine{cfg: config.Config{Speculative: speculative}, units: map[string]*unit{u.k: u}, byRole: map[string]*unit{catalog.RoleBrain: u},
		evals: &evalCache{path: path}, log: log.New(io.Discard, "", 0)}
}

func TestSpeculationNeedsTheOperatorAndTheEvidence(t *testing.T) {
	proven := map[string]evals.Score{"m": {N: 100, Correct: 80}, SpecName("m"): {N: 100, Correct: 92}}
	unproven := map[string]evals.Score{"m": {N: 100, Correct: 90}, SpecName("m"): {N: 100, Correct: 90}}
	for _, tc := range []struct {
		name, setting string
		scores        map[string]evals.Score
		want          string
	}{
		{"not allowed", "", proven, ""},
		{"allowed, proven", config.SpeculativeNgram, proven, SpecNgram},
		{"allowed, as good but not proven", config.SpeculativeNgram, unproven, ""},
		{"allowed, never measured", config.SpeculativeNgram, map[string]evals.Score{"m": {N: 100, Correct: 80}}, ""},
	} {
		e := specEngine(t, tc.setting, tc.scores)
		e.applySpeculative()
		if got := e.units["m|src"].llama.Spec; got != tc.want {
			t.Errorf("%s: spec %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTheSpeculativeTwinIsServedToMeasureIt(t *testing.T) {
	e := specEngine(t, "", nil)
	bc, err := e.route(Request{Model: SpecName("m")})
	if err != nil {
		t.Fatal(err)
	}
	if bc.local == nil || bc.local.llama.Spec != SpecNgram || bc.name != SpecName("m") || bc.local == e.units["m|src"] {
		t.Fatalf("twin %+v", bc)
	}
	if e.units["m|src"].llama.Spec != "" {
		t.Fatal("making the twin turned speculation on for the model itself")
	}
	if again := e.specVariant(SpecName("m")); again != bc.local {
		t.Fatal("a second request made a second twin")
	}
	if !e.owns(bc.local) {
		t.Fatal("the engine does not own its twin, so closing it would leave the server running")
	}
	if ident(bc.local) == ident(e.units["m|src"]) {
		t.Fatal("the twin shares its model's residency entry")
	}
	if _, err := e.route(Request{Model: SpecName("nothing")}); err == nil {
		t.Fatal("a twin of a model not installed was served")
	}
}
