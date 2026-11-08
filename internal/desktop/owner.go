// Which control plane created a container (OwnerLabel), so that two control planes on one Docker - a dev box, the
// owner's own host running a second one beside the app's - never pause, stop or sweep each other's desktops.
package desktop

import (
	"log"
	"sync"

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
