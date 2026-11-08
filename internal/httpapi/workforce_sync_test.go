// Tests for the 2026-11-07 Workforce sync on the control plane's side: a handoff's role or skill finds its holders
// ignoring case (as the queue and policies do), and the policy dry run that SetAccess wires reads the same union of
// registry and access roles as the gate.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/skillhub"
)

// TestHandoffHoldersIgnoreCase checks skillHubFinder matches a role and a skill name whatever their case.
func TestHandoffHoldersIgnoreCase(t *testing.T) {
	hub, err := skillhub.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.SetWorkerRoles("w2", []string{"Reviewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.SetWorkerRoles("w1", []string{"clerk"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"format": skillhub.BundleFormat, "exported_at": 1,
		"skill": map[string]any{"name": "enter-bill", "status": "ready", "steps": []any{map[string]any{"n": 1, "kind": "click"}}}})
	if _, _, err := hub.Publish("enter-bill", raw, "w3", ""); err != nil {
		t.Fatal(err)
	}
	if err := hub.RecordInstall("w3", "enter-bill", 1); err != nil { // the publish handler records the publisher so
		t.Fatal(err)
	}

	finder := skillHubFinder{hub}
	for _, role := range []string{"reviewer", "REVIEWER", "Reviewer"} {
		if got, err := finder.Holders(context.Background(), role, ""); err != nil || !slices.Equal(got, []string{"w2"}) {
			t.Errorf("role %q: holders %v, %v", role, got, err)
		}
	}
	if got, err := finder.Holders(context.Background(), "", "Enter-Bill"); err != nil || !slices.Equal(got, []string{"w3"}) {
		t.Errorf("skill Enter-Bill: holders %v, %v", got, err)
	}
	if got, _ := finder.Holders(context.Background(), "auditor", ""); len(got) != 0 {
		t.Errorf("an unknown role found holders: %v", got)
	}
}

// TestPolicyDryRunReadsTheGatesWorkerRoles checks /api/access/policies/check, as SetAccess wires it, matches a
// worker_role the worker carries only in the skill registry, as the gate does.
func TestPolicyDryRunReadsTheGatesWorkerRoles(t *testing.T) {
	s, store := accessServer(t)
	hub, err := skillhub.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	s.SkillHub = hub
	if _, err := hub.SetWorkerRoles("w1", []string{"Filing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePolicy(access.Policy{WorkerRole: "filing", ActionMatch: `\bsubmit\b`, ApproverRoles: []string{"partner"}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterAccessLocalRoutes(mux)

	body, _ := json.Marshal(map[string]any{"worker": "w1", "action": "submit the return"})
	req := httptest.NewRequest(http.MethodPost, "/api/access/policies/check", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:40000"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "partner") {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	if roles := s.workerRoles("w1"); !slices.Contains(roles, "Filing") {
		t.Fatalf("the gate's roles = %v", roles)
	}
}
