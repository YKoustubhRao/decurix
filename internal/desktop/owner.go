// Which control plane created a container (OwnerLabel), so that two control planes on one Docker - a dev box, the
// owner's own host running a second one beside the app's - never pause, stop or sweep each other's desktops.
package desktop

import (
	"context"
	"crypto/hmac"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// OwnerLabel holds, on every desktop and practice container (and practice network) a control plane creates, that
// control plane's instance id. Found live (2026-11-08): a second control plane on the same Docker idle-paused a
// desktop the first was driving mid-task, and its start-up cleanup removed every practice desktop on the host.
const OwnerLabel = "decurix.owner"

// ownerID is this control plane's instance id (its database's, sqlitedb.InstanceID): the value of OwnerLabel on what
// it creates. A variable so tests can name another.
var ownerID = defaultOwnerID

// defaultOwnerID is sqlitedb.InstanceID.
var defaultOwnerID = sqlitedb.InstanceID

// OwnedHere reports whether labels say this control plane created the container.
func OwnedHere(labels map[string]string) bool {
	owner := labels[OwnerLabel]
	return owner != "" && owner == ownerID()
}

// ForeignOwner reports whether labels name another control plane as the container's creator. A container with no
// owner label (made before the label existed) is no other's: it stays listed, and is left to no sweep (OwnedHere).
func ForeignOwner(labels map[string]string) bool {
	owner := labels[OwnerLabel]
	return owner != "" && owner != ownerID()
}

// notOwnedSaid keeps which (sweep, container) pairs LogNotOwned has said already.
var notOwnedSaid sync.Map

// LogNotOwned says once, per sweep and container, that a sweep left a container alone because this control plane did
// not create it (no owner label, or another's).
func LogNotOwned(sweep, id string) {
	if _, said := notOwnedSaid.LoadOrStore(sweep+"\x00"+id, true); !said {
		log.Printf("%s: leaving %s alone - this control plane did not create it (no %s label of its own)", sweep, id, OwnerLabel)
	}
}

// owned reports whether container c (desktop id) is this control plane's: its owner label says so, or, made before
// the label, the agent token it was given at creation is the one this control plane issues for id (ownedLegacy).
func (m *Manager) owned(ctx context.Context, c container.Summary, id string) bool {
	if c.Labels[OwnerLabel] != "" {
		return OwnedHere(c.Labels)
	}
	if v, ok := m.legacyOwned.Load(c.ID); ok {
		return v.(bool)
	}
	mine := m.ownedLegacy(ctx, c.ID, id)
	m.legacyOwned.Store(c.ID, mine)
	return mine
}

// ownedLegacy adopts a desktop made before OwnerLabel when the agent token in its creation environment is the one
// this control plane derives for it (AgentToken: an HMAC of the desktop's id under this control plane's own secret),
// a proof no other control plane can make. Docker cannot label a container after it is made; without this, upgrading
// left every existing desktop to no idle pause. A container that cannot be read, or has no token, is not adopted.
func (m *Manager) ownedLegacy(ctx context.Context, containerID, id string) bool {
	want := m.AgentToken(id)
	if want == "" {
		return false
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	inspect, err := m.client.ContainerInspect(inspectCtx, containerID)
	if err != nil || inspect.Config == nil {
		return false
	}
	return tokenIn(inspect.Config.Env, want)
}

// tokenIn reports whether env (a creation environment, NAME=value) holds AgentTokenEnv with the value want.
func tokenIn(env []string, want string) bool {
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, AgentTokenEnv+"="); ok {
			return hmac.Equal([]byte(value), []byte(want))
		}
	}
	return false
}
