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

	// Crash says why a desktop that is not running stopped on its own ("it
	// exited with code 1", "it ran out of memory"), or "" when it is running,
	// paused, or was stopped the way Stop stops it. Status stays "stopped"
	// for a crashed desktop, so nothing that reads Status changes meaning;
	// the dashboard shows this as the red light on its card.
	Crash string `json:"crash,omitempty"`

	CPUs     int64 `json:"cpus"`
	MemoryMB int64 `json:"memory_mb"`

	// Specialization is a one-line description of what this desktop was
	// created to focus on, set at creation time and injected into the
	// agent's identity context (see agent/decurix_agent/identity.py). "" for
	// a desktop with no particular focus.
	Specialization string `json:"specialization,omitempty"`

	// ParentID is the desktop this one was spawned as a specialized child
	// of (see httpapi.createDesktopHandler), or "" for a desktop created
	// standalone. Set once at creation and never changed afterward.
	ParentID string `json:"parent_id,omitempty"`
}

// ResourceAllocation represents a container's CPU and memory allocation.
type ResourceAllocation struct {
	CPUs     int64 `json:"cpus"`
	MemoryMB int64 `json:"memory_mb"`
}

// ProvisioningDesktop tracks an in-flight desktop creation so that callers
// receive immediate acceptance (<15ms) while the volume, container, host apps,
// and VNC initialization finish asynchronously in the background.
type ProvisioningDesktop struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Profile        string `json:"profile"`
	Harness        string `json:"harness"`
	Model          string `json:"model,omitempty"`
	CPUs           int64  `json:"cpus"`
	MemoryMB       int64  `json:"memory_mb"`
	Status         string `json:"status"` // "provisioning", "running", "failed"
	Stage          string `json:"stage"`  // "initializing", "volume", "container", "apps", "starting", "readiness", "ready"
	Message        string `json:"message"`
	Progress       int    `json:"progress"` // percentage 0-100
	URL            string `json:"url"`
	CreatedAt      string `json:"created_at"`
	Error          string `json:"error,omitempty"`
	Specialization string `json:"specialization,omitempty"`
	ParentID       string `json:"parent_id,omitempty"`
	Ready          bool   `json:"ready"`
}

// ToDesktop converts a ProvisioningDesktop into a Desktop struct.
func (p *ProvisioningDesktop) ToDesktop() Desktop {
	return Desktop{
		ID:             p.ID,
		Name:           p.Name,
		Profile:        p.Profile,
		Harness:        p.Harness,
		Model:          p.Model,
		Volume:         "desktop_" + p.ID + "_home",
		Status:         p.Status,
		CPUs:           p.CPUs,
		MemoryMB:       p.MemoryMB,
		URL:            p.URL,
		CreatedAt:      p.CreatedAt,
		Specialization: p.Specialization,
		ParentID:       p.ParentID,
	}
}

