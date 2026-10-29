package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

var person = Source{Kind: SourcePerson, Ref: "owner"}

func mustAdd(t *testing.T, s *Store, scope, key, kind, text string) Item {
	t.Helper()

	it, err := s.Add(Item{Scope: scope, ScopeKey: key, Kind: kind, Text: text, Source: person})
	if err != nil {
		t.Fatalf("Add(%s %s): %v", scope, text, err)
	}

	return it
}

func texts(items []Item) string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Text)
	}

	return strings.Join(out, "|")
}

func TestForWorkerOrdersOrgRoleWorkerAndSkipsOthers(t *testing.T) {
	s := newStore(t)

	mustAdd(t, s, ScopeWorker, "w1", KindLesson, "worker one")
	mustAdd(t, s, ScopeRole, "clerk", KindRule, "clerk rule")
	mustAdd(t, s, ScopeOrg, "", KindRule, "org rule")
	mustAdd(t, s, ScopeRole, "auditor", KindFact, "auditor fact")
	mustAdd(t, s, ScopeRole, "other", KindRule, "not mine")
	mustAdd(t, s, ScopeWorker, "w2", KindRule, "worker two")
	mustAdd(t, s, ScopeTask, "t1", KindCorrection, "task one")

	got, err := s.ForWorker("w1", []string{"auditor", "clerk", "auditor"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "org rule|auditor fact|clerk rule|worker one"; texts(got) != want {
		t.Fatalf("ForWorker = %s, want %s", texts(got), want)
	}

	task, err := s.ForTask("t1")
	if err != nil || texts(task) != "task one" {
		t.Fatalf("ForTask = %s %v", texts(task), err)
	}

	if line := got[1].Line(); line != "Fact (role auditor): auditor fact" {
		t.Fatalf("Line = %q", line)
	}
	if line := got[0].Line(); line != "Rule (org): org rule" {
		t.Fatalf("Line = %q", line)
	}
}

func TestAddValidates(t *testing.T) {
	s := newStore(t)

	cases := []struct {
		name string
		it   Item
	}{
		{"bad scope", Item{Scope: "galaxy", Kind: KindRule, Text: "x", Source: person}},
		{"org with key", Item{Scope: ScopeOrg, ScopeKey: "k", Kind: KindRule, Text: "x", Source: person}},
		{"role without key", Item{Scope: ScopeRole, Kind: KindRule, Text: "x", Source: person}},
		{"bad kind", Item{Scope: ScopeOrg, Kind: "whim", Text: "x", Source: person}},
		{"empty text", Item{Scope: ScopeOrg, Kind: KindRule, Text: "  ", Source: person}},
		{"long text", Item{Scope: ScopeOrg, Kind: KindRule, Text: strings.Repeat("a", MaxText+1), Source: person}},
		{"no source", Item{Scope: ScopeOrg, Kind: KindRule, Text: "x"}},
		{"bad source", Item{Scope: ScopeOrg, Kind: KindRule, Text: "x", Source: Source{Kind: "rumour"}}},
		{"control in ref", Item{Scope: ScopeOrg, Kind: KindRule, Text: "x", Source: Source{Kind: SourceRun, Ref: "a\nb"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Add(c.it); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestSupersedeAndWhy(t *testing.T) {
	s := newStore(t)

	first := mustAdd(t, s, ScopeRole, "clerk", KindRule, "round to two places")
	second, err := s.Supersede(first.ID, Item{Text: "round to whole units", Source: Source{Kind: SourceRun, TaskID: "t7", Worker: "w1"}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Scope != ScopeRole || second.ScopeKey != "clerk" || second.Kind != KindRule {
		t.Fatalf("replacement lost the scope or kind: %+v", second)
	}
	third, err := s.Supersede(second.ID, Item{Kind: KindCorrection, Text: "round half up to whole units", Source: person})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Supersede(first.ID, Item{Text: "again", Source: person}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("superseding a replaced item: %v", err)
	}
	if _, err := s.Supersede("kn_missing", Item{Text: "x", Source: person}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseding a missing item: %v", err)
	}

	live, _ := s.ForWorker("w1", []string{"clerk"})
	if texts(live) != "round half up to whole units" {
		t.Fatalf("live = %s", texts(live))
	}

	it, chain, err := s.Why(third.ID)
	if err != nil || it.ID != third.ID || len(chain) != 2 || chain[0].ID != second.ID || chain[1].ID != first.ID {
		t.Fatalf("Why = %+v %+v %v", it, chain, err)
	}
	if chain[0].Source.TaskID != "t7" || chain[0].SupersededBy != third.ID {
		t.Fatalf("chain lost its source: %+v", chain[0])
	}

	if _, _, err := s.Why("kn_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Why missing: %v", err)
	}

	if err := s.Retire(third.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(third.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retiring twice: %v", err)
	}
	if live, _ := s.ForWorker("w1", []string{"clerk"}); len(live) != 0 {
		t.Fatalf("retired item still given: %s", texts(live))
	}
	if _, err := s.Supersede(third.ID, Item{Text: "x", Source: person}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("superseding a retired item: %v", err)
	}
}

type fakeRoles map[string][]string

func (f fakeRoles) WorkerRoles(worker string) ([]string, error) { return f[worker], nil }

func call(t *testing.T, mux *http.ServeMux, verb, path string, body any) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(verb, path, reader))

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: not JSON: %s", verb, path, rec.Body.String())
	}

	return rec.Code, out
}

func TestHandler(t *testing.T) {
	h := &Handler{Store: newStore(t), Roles: fakeRoles{"w1": {"clerk"}}}
	mux := http.NewServeMux()
	h.Register(mux)

	code, out := call(t, mux, "POST", "/api/knowledge", map[string]any{
		"scope": "org", "kind": "rule", "text": "Never pay an unapproved invoice.", "source": map[string]any{"kind": "person", "ref": "owner"},
	})
	if code != 200 || out["id"] == "" {
		t.Fatalf("add = %d %v", code, out)
	}
	orgID := out["id"].(string)

	code, out = call(t, mux, "POST", "/api/knowledge", map[string]any{
		"scope": "role", "scope_key": "clerk", "kind": "fact", "text": "Ledgers close on the last working day.",
		"source": map[string]any{"kind": "demonstration", "ref": "rec-1", "worker": "w2"},
	})
	if code != 200 {
		t.Fatalf("add role = %d %v", code, out)
	}
	call(t, mux, "POST", "/api/knowledge", map[string]any{
		"scope": "task", "scope_key": "t9", "kind": "correction", "text": "Use the second sheet.", "source": map[string]any{"kind": "person"},
	})

	code, out = call(t, mux, "GET", "/api/knowledge?worker=w1&task=t9", nil)
	lines := out["lines"].([]any)
	if code != 200 || len(lines) != 3 || lines[0] != "Rule (org): Never pay an unapproved invoice." ||
		lines[1] != "Fact (role clerk): Ledgers close on the last working day." || lines[2] != "Correction (task): Use the second sheet." {
		t.Fatalf("for worker = %d %v", code, out)
	}

	code, out = call(t, mux, "POST", "/api/knowledge/supersede", map[string]any{
		"id": orgID, "text": "Never pay an invoice no person approved.", "source": map[string]any{"kind": "person"},
	})
	if code != 200 {
		t.Fatalf("supersede = %d %v", code, out)
	}
	newID := out["item"].(map[string]any)["id"].(string)

	code, out = call(t, mux, "GET", "/api/knowledge/why?id="+newID, nil)
	if code != 200 || len(out["replaced"].([]any)) != 1 {
		t.Fatalf("why = %d %v", code, out)
	}

	code, out = call(t, mux, "POST", "/api/knowledge/supersede", map[string]any{"id": newID, "retire": true})
	if code != 200 || out["retired"] != true {
		t.Fatalf("retire = %d %v", code, out)
	}

	cases := []struct {
		name, verb, path string
		body             any
		code             int
	}{
		{"bad scope", "POST", "/api/knowledge", map[string]any{"scope": "x", "kind": "rule", "text": "t", "source": map[string]any{"kind": "person"}}, 400},
		{"unknown field", "POST", "/api/knowledge", map[string]any{"scope": "org", "nope": 1}, 400},
		{"supersede missing", "POST", "/api/knowledge/supersede", map[string]any{"id": "kn_x", "text": "t", "source": map[string]any{"kind": "person"}}, 404},
		{"supersede no id", "POST", "/api/knowledge/supersede", map[string]any{"text": "t"}, 400},
		{"why missing", "GET", "/api/knowledge/why?id=kn_x", nil, 404},
		{"bad worker", "GET", "/api/knowledge?worker=%3Cx%3E", nil, 400},
		{"wrong method", "DELETE", "/api/knowledge", nil, 405},
		{"why wrong method", "POST", "/api/knowledge/why", map[string]any{}, 405},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, out := call(t, mux, c.verb, c.path, c.body); code != c.code {
				t.Fatalf("code = %d, want %d (%v)", code, c.code, out)
			}
		})
	}
}

// Without a role source a worker still gets org and its own items.
func TestHandlerWithoutRoles(t *testing.T) {
	h := &Handler{Store: newStore(t)}
	mux := http.NewServeMux()
	h.Register(mux)

	mustAdd(t, h.Store, ScopeOrg, "", KindRule, "org")
	mustAdd(t, h.Store, ScopeWorker, "w1", KindLesson, "mine")

	code, out := call(t, mux, "GET", "/api/knowledge?worker=w1", nil)
	if code != 200 || len(out["lines"].([]any)) != 2 {
		t.Fatalf("= %d %v", code, out)
	}
}
