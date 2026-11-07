package desktop

// Desktop represents a managed desktop container: its identity, profile, backing Docker resources, status, and resource allocation.
type Desktop struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Harness string `json:"harness"`
	// Brain is the model TheBot reasons with: BrainOllama (the local model,
	// the default), BrainClaude, BrainAzure or BrainOpenAI (hosted, reached through the
	// control plane's /api/llm proxy). Fixed at creation, like Harness.
	Brain       string `json:"brain"`
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

	// SharedFolder is the one folder of the person's computer this desktop may read and write, as they wrote it
	// (a path like C:/Users/me/Documents/Shared), or "" for none. Fixed at creation, like the mount behind it.
	SharedFolder string `json:"shared_folder,omitempty"`

	// SharedNetwork is a desktop made before desktops had a network each: still on the shared decurix-network with
	// the older desktops, so not kept apart from them until it is recreated (its card says so).
	SharedNetwork bool `json:"shared_network,omitempty"`

	// ParentID is the desktop this one was spawned as a specialized child
	// of (see httpapi.createDesktopHandler), or "" for a desktop created
	// standalone. Set once at creation and never changed afterward.
	ParentID string `json:"parent_id,omitempty"`

	// MissingApps are the host apps someone asked this desktop to have that it does not, each with why in plain
	// words; Pending ones are still being tried. Kept by the Manager (see missingapps.go), not by Docker.
	MissingApps []MissingApp `json:"missing_apps,omitempty"`

	// RestReason is set on a stopped desktop the control plane stopped to save memory, never on one a person
	// stopped: RestIdle ("idle", it stayed paused past DECURIX_IDLE_STOP_AFTER_MINUTES) or RestRoom ("memory", stopped
	// to make room for another desktop's start). Its home is kept and it starts again by itself when given work, so a
	// card says "Resting to save memory - starts when given work". RestNote is one line saying which, RestedAt when
	// (RFC3339). Kept in memory: a restarted control plane shows such a desktop as plainly stopped.
	RestReason string `json:"rest_reason,omitempty"`
	RestNote   string `json:"rest_note,omitempty"`
	RestedAt   string `json:"rested_at,omitempty"`
}

// MissingApp is a host app someone asked a desktop to have that is not on it, and why.
type MissingApp struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
	// Pending is true while Decurix is still trying: the host could not be asked about it yet, which is not a "no".
	Pending bool `json:"pending,omitempty"`
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
	// creation names this one creation; its home volume carries it (homeCreationLabel).
	creation       string
	ID             string `json:"id"`
	Name           string `json:"name"`
	Profile        string `json:"profile"`
	Harness        string `json:"harness"`
	Brain          string `json:"brain"`
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
	SharedFolder   string `json:"shared_folder,omitempty"`
	Ready          bool   `json:"ready"`
	// MissingApps are the requested host apps that are not (or not yet) on the desktop, and why.
	MissingApps []MissingApp `json:"missing_apps,omitempty"`
}

// ToDesktop converts a ProvisioningDesktop into a Desktop struct.
func (p *ProvisioningDesktop) ToDesktop() Desktop {
	return Desktop{
		ID:             p.ID,
		Name:           p.Name,
		Profile:        p.Profile,
		Harness:        p.Harness,
		Brain:          p.Brain,
		Model:          p.Model,
		Volume:         "desktop_" + p.ID + "_home",
		Status:         p.Status,
		CPUs:           p.CPUs,
		MemoryMB:       p.MemoryMB,
		URL:            p.URL,
		CreatedAt:      p.CreatedAt,
		Specialization: p.Specialization,
		SharedFolder:   p.SharedFolder,
		ParentID:       p.ParentID,
		MissingApps:    p.MissingApps,
	}
}
