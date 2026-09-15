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

	CPUs     int64 `json:"cpus"`
	MemoryMB int64 `json:"memory_mb"`
}
