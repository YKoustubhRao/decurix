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
}
