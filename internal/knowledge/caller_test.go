package knowledge

import (
	"net/http"
	"strings"
	"testing"
)

// A desktop asking for another worker's or another task's knowledge gets only its own scope (C-09).
func TestADesktopReadsOnlyItsOwnScope(t *testing.T) {
	s := newStore(t)
	mustAdd(t, s, ScopeOrg, "", "rule", "Org rule.")
	mustAdd(t, s, ScopeWorker, "mine", "fact", "My fact.")
	mustAdd(t, s, ScopeWorker, "theirs", "fact", "Their fact.")
	mustAdd(t, s, ScopeTask, "t-theirs", "correction", "Their task's correction.")
	mustAdd(t, s, ScopeTask, "t-mine", "correction", "My task's correction.")

	h := &Handler{Store: s,
		Caller: func(*http.Request) (string, bool) { return "mine", true },
		TaskOf: func(worker, task string) bool { return worker == "mine" && task == "t-mine" },
	}
	mux := http.NewServeMux()
	h.Register(mux)

	code, out := call(t, mux, "GET", "/api/knowledge?worker=theirs", nil)
	got := joinLines(out)
	if code != 200 || out["worker"] != "mine" || strings.Contains(got, "Their") || !strings.Contains(got, "My fact.") {
		t.Fatalf("another worker's scope: %d %v", code, out)
	}

	if code, out := call(t, mux, "GET", "/api/knowledge?task=t-theirs", nil); code != http.StatusForbidden {
		t.Fatalf("another task's knowledge: %d %v", code, out)
	}

	code, out = call(t, mux, "GET", "/api/knowledge?task=t-mine", nil)
	if code != 200 || !strings.Contains(joinLines(out), "My task's correction.") {
		t.Fatalf("own task: %d %v", code, out)
	}
}

// A person (no desktop caller) reads any scope, as before.
func TestAPersonReadsAnyScope(t *testing.T) {
	s := newStore(t)
	mustAdd(t, s, ScopeWorker, "theirs", "fact", "Their fact.")
	h := &Handler{Store: s, Caller: func(*http.Request) (string, bool) { return "", false }}
	mux := http.NewServeMux()
	h.Register(mux)
	if code, out := call(t, mux, "GET", "/api/knowledge?worker=theirs", nil); code != 200 || !strings.Contains(joinLines(out), "Their fact.") {
		t.Fatalf("%d %v", code, out)
	}
}

func joinLines(out map[string]any) string {
	var b []string
	for _, l := range out["lines"].([]any) {
		b = append(b, l.(string))
	}
	return strings.Join(b, "|")
}
