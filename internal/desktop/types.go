package desktop

// Desktop represents a managed desktop container: its identity, profile, backing Docker resources, status, and resource allocation.
type Desktop struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Profile     string `json:"profile"`
	Harness     string `json:"harness"`
	Model       string `json:"model,omitempty"`
	ContainerID string `json:"container_id"`
	Volume      string `json:"volume"`
	Status      string `json:"status"`
	URL         string `json:"url"`
	CreatedAt   string `json:"created_at"`

	// StartedAt is Docker's own record of when the container last transitioned
	// to running (RFC3339Nano), read straight off container inspect - not
	// updated by anything in this package. "" for a container that has never
	// started. Used by internal/idlepause as the floor for how long a
	// desktop has been idle: a desktop just started (or restarted) has not
	// been idle since some old task, whatever its task history says.
	StartedAt string `json:"started_at,omitempty"`

	CPUs     int64 `json:"cpus"`
	MemoryMB int64 `json:"memory_mb"`

	// Specialization is a one-line description of what this desktop was
	// created to focus on, set at creation time and injected into the
	// agent's identity context (see agent/flowera_agent/identity.py). "" for
	// a desktop with no particular focus.
	Specialization string `json:"specialization,omitempty"`

	// ParentID is the desktop this one was spawned as a specialized child
	// of (see httpapi.createDesktopHandler), or "" for a desktop created
	// standalone. Set once at creation and never changed afterward.
	ParentID string `json:"parent_id,omitempty"`
}
