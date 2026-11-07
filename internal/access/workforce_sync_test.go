// Tests for the 2026-11-07 Workforce sync: the policy dry run reads a worker's roles the way the gate does
// (Handler.WorkerRoles), and HasUsers says when a gated reply starts needing a sign-in.
package access

import (
	"net/http"
	"strings"
	"testing"
)

// TestPolicyCheckUsesTheGatesWorkerRoles checks the dry run matches a worker role the gate's union carries (here a
// skill-registry role this store does not hold), and still reads this store's roles with no hook.
func TestPolicyCheckUsesTheGatesWorkerRoles(t *testing.T) {
	h, mux := newHandler(t, false)
	if _, err := h.Store.SavePolicy(Policy{WorkerRole: "filing", ActionMatch: `\bsubmit\b`, ApproverRoles: []string{"partner"}}); err != nil {
		t.Fatal(err)
	}
	check := func() string {
		rec := do(mux, http.MethodPost, "/api/access/policies/check", map[string]any{"worker": "w1", "action": "submit the return"})
		if rec.Code != http.StatusOK {
			t.Fatalf("check: %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	if body := check(); strings.Contains(body, "partner") {
		t.Fatalf("no role anywhere, yet the policy applied: %s", body)
	}

	// The registry gives w1 "Filing"; the gate's union (Server.workerRoles) carries it, so the dry run must too.
	h.WorkerRoles = func(worker string) []string {
		if worker == "w1" {
			return []string{"Filing"}
		}
		return nil
	}
	if body := check(); !strings.Contains(body, "partner") {
		t.Fatalf("the registry role did not reach the dry run: %s", body)
	}

	// Without the hook, this store's own roles are read, as before.
	h.WorkerRoles = nil
	if err := h.Store.SetWorkerRoles("w1", []string{"filing"}); err != nil {
		t.Fatal(err)
	}
	if body := check(); !strings.Contains(body, "partner") {
		t.Fatalf("the store's role did not reach the dry run: %s", body)
	}

	// worker_roles given in the body still decide on their own.
	rec := do(mux, http.MethodPost, "/api/access/policies/check", map[string]any{"worker": "w1", "action": "submit it", "worker_roles": []string{}})
	if strings.Contains(rec.Body.String(), "partner") {
		t.Fatalf("explicit empty worker_roles still matched: %s", rec.Body)
	}
}

// TestHasUsers checks HasUsers is false on a new store and true from the first account on, disabled or not.
func TestHasUsers(t *testing.T) {
	s := newStore(t)
	if has, err := s.HasUsers(); err != nil || has {
		t.Fatalf("new store: %v %v", has, err)
	}
	u, err := s.CreateUser("asha", "password-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasUsers(); err != nil || !has {
		t.Fatalf("with a (disabled) user: %v %v", has, err)
	}
}
