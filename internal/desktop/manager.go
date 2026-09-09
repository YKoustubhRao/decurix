package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Manager owns the Docker client and the desktop containers it manages.
type Manager struct {
	client *client.Client

	mu sync.Mutex
}

// NewManager creates a Manager backed by the given Docker client.
func NewManager(cli *client.Client) *Manager {
	return &Manager{client: cli}
}

// Create provisions a new desktop container (volume, env, container config) for the given profile and starts it, rolling back on failure.
func (m *Manager) Create(
	ctx context.Context,
	name string,
	profile string,
	harness string,
	cpus int64,
	memoryMB int64,
) (*Desktop, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	imageName, err := resolveDesktopImage(profile)
	if err != nil {
		return nil, err
	}

	volumeName := fmt.Sprintf(
		"desktop_%s_home",
		sanitizeName(name),
	)

	// Create persistent home volume.
	_, err = m.client.VolumeCreate(
		ctx,
		volume.CreateOptions{
			Name: volumeName,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create volume: %w",
			err,
		)
	}

	containerName := fmt.Sprintf(
		"desktop_%s",
		sanitizeName(name),
	)

	env := buildDesktopEnv(name, harness)

	config, hostConfig, networkingConfig := buildContainerConfigs(
		imageName,
		containerName,
		profile,
		harness,
		cpus,
		memoryMB,
		env,
		volumeName,
	)

	resp, err := m.client.ContainerCreate(
		ctx,
		config,
		hostConfig,
		networkingConfig,
		nil,
		containerName,
	)
	if err != nil {
		_ = m.client.VolumeRemove(
			ctx,
			volumeName,
			true,
		)

		return nil, fmt.Errorf(
			"create container: %w",
			err,
		)
	}

	if err := m.client.ContainerStart(
		ctx,
		resp.ID,
		container.StartOptions{},
	); err != nil {

		_ = m.client.ContainerRemove(
			ctx,
			resp.ID,
			container.RemoveOptions{
				Force: true,
			},
		)

		_ = m.client.VolumeRemove(
			ctx,
			volumeName,
			true,
		)

		return nil, fmt.Errorf(
			"start container: %w",
			err,
		)
	}

	return &Desktop{
		ID:          sanitizeName(name),
		Name:        name,
		Profile:     profile,
		Harness:     harness,
		ContainerID: resp.ID,
		Volume:      volumeName,
		Status:      "running",
		CPUs:        cpus,
		MemoryMB:    memoryMB,
		URL: fmt.Sprintf(
			"http://%s:8080/desktop/%s/vnc.html",
			ServerHost(),
			sanitizeName(name),
		),
		CreatedAt: time.Now().Format(time.RFC3339),
	}, nil
}

// List returns all desktop containers managed by this instance, read from their Docker labels and inspect data.
func (m *Manager) List(
	ctx context.Context,
) ([]Desktop, error) {

	args := filters.NewArgs()

	args.Add(
		"label",
		"desktop-manager=true",
	)

	containers, err := m.client.ContainerList(
		ctx,
		container.ListOptions{
			All:     true,
			Filters: args,
		},
	)
	if err != nil {
		return nil, err
	}

	result := make([]Desktop, 0, len(containers))

	for _, c := range containers {

		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(
				c.Names[0],
				"/",
			)
		}

		volumeName := ""
		var cpus int64
		var memoryMB int64

		inspect, err := m.client.ContainerInspect(
			ctx,
			c.ID,
		)

		if err == nil {
			for _, mount := range inspect.Mounts {
				if mount.Destination == "/home/headless" {
					volumeName = mount.Name
					break
				}
			}

			if inspect.HostConfig != nil {
				if inspect.HostConfig.NanoCPUs > 0 {
					cpus = inspect.HostConfig.NanoCPUs / 1_000_000_000
				}

				if inspect.HostConfig.Memory > 0 {
					memoryMB = inspect.HostConfig.Memory / (1024 * 1024)
				}
			}
		}

		profile := c.Labels["desktop-profile"]

		if profile == "" {
			profile = "basic"
		}

		harness := c.Labels["desktop-harness"]

		if harness == "" {
			harness = HarnessOllama
		}

		status := "stopped"

		if c.State == "running" {
			status = "running"
		}

		id := strings.TrimPrefix(
			name,
			"desktop_",
		)

		createdAt := ""
		if c.Created > 0 {
			createdAt = time.Unix(
				c.Created,
				0,
			).Format(time.RFC3339)
		}

		url := fmt.Sprintf(
			"http://%s:8080/desktop/%s/vnc.html",
			ServerHost(),
			id,
		)

		result = append(
			result,
			Desktop{
				ID:          id,
				Name:        id,
				Profile:     profile,
				Harness:     harness,
				ContainerID: c.ID,
				Volume:      volumeName,
				Status:      status,
				URL:         url,
				CreatedAt:   createdAt,
				CPUs:        cpus,
				MemoryMB:    memoryMB,
			},
		)
	}

	return result, nil
}

// Start starts the Docker container backing the given desktop ID.
func (m *Manager) Start(
	ctx context.Context,
	id string,
) error {

	name := "desktop_" + sanitizeName(id)

	return m.client.ContainerStart(
		ctx,
		name,
		container.StartOptions{},
	)
}

// Stop stops the Docker container backing the given desktop ID.
func (m *Manager) Stop(
	ctx context.Context,
	id string,
) error {

	name := "desktop_" + sanitizeName(id)

	timeoutEnv := os.Getenv("DESKTOP_STOP_TIMEOUT_SECONDS")
	timeout := 10
	if timeoutEnv != "" {
		if parsedTimeout, err := strconv.Atoi(timeoutEnv); err == nil && parsedTimeout > 0 {
			timeout = parsedTimeout
		}
	}

	return m.client.ContainerStop(
		ctx,
		name,
		container.StopOptions{
			Timeout: &timeout,
		},
	)
}

// Restart stops and restarts the Docker container backing the given desktop ID.
func (m *Manager) Restart(
	ctx context.Context,
	id string,
) error {

	name := "desktop_" + sanitizeName(id)

	timeout := 10

	if err := m.client.ContainerStop(
		ctx,
		name,
		container.StopOptions{
			Timeout: &timeout,
		},
	); err != nil {
		// Container may already be stopped.
	}

	return m.client.ContainerStart(
		ctx,
		name,
		container.StartOptions{},
	)
}

// Delete removes the Docker container and persistent volume backing the given desktop ID.
func (m *Manager) Delete(
	ctx context.Context,
	id string,
) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	name := "desktop_" + sanitizeName(id)

	inspect, err := m.client.ContainerInspect(
		ctx,
		name,
	)
	if err != nil {
		return err
	}

	var volumeName string

	for _, mount := range inspect.Mounts {
		if mount.Destination == "/home/headless" {
			volumeName = mount.Name
			break
		}
	}

	err = m.client.ContainerRemove(
		ctx,
		name,
		container.RemoveOptions{
			Force: true,
		},
	)
	if err != nil {
		return err
	}

	// Delete persistent storage too.
	if volumeName != "" {
		if err := m.client.VolumeRemove(
			ctx,
			volumeName,
			true,
		); err != nil {
			log.Printf(
				"warning: removing volume %s: %v",
				volumeName,
				err,
			)
		}
	}

	return nil
}

// SendMessage posts a JSON message to a running desktop's internal message endpoint.
func (m *Manager) SendMessage(
	ctx context.Context,
	id string,
	content string,
) error {

	desktopID := sanitizeName(id)
	name := "desktop_" + desktopID

	// Verify that the desktop container exists.
	inspect, err := m.client.ContainerInspect(
		ctx,
		name,
	)
	if err != nil {
		return fmt.Errorf("find desktop: %w", err)
	}

	// Make sure the desktop is running.
	if inspect.State == nil || !inspect.State.Running {
		return fmt.Errorf("desktop %s is not running", desktopID)
	}

	payload := map[string]string{
		"content": content,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}

	// Communicate directly over the Docker network.
	// vm_server.py listens on port 8081 inside the desktop container.
	url := fmt.Sprintf(
		"http://%s:8081/message",
		name,
	)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		strings.NewReader(string(body)),
	)
	if err != nil {
		return fmt.Errorf("create message request: %w", err)
	}

	req.Header.Set(
		"Content-Type",
		"application/json",
	)

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf(
			"send message to desktop %s: %w",
			desktopID,
			err,
		)
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf(
			"desktop %s returned HTTP %d",
			desktopID,
			resp.StatusCode,
		)
	}

	return nil
}

// PersistMemoryFile writes pre-formatted memory content into a desktop container's memory directory.
func (m *Manager) PersistMemoryFile(
	ctx context.Context,
	desktopID string,
	filename string,
	fileContent string,
) error {

	containerName := "desktop_" + sanitizeName(desktopID)
	memoryDir := "/home/headless/memory"
	delimiter := fmt.Sprintf("FLOWERA_MEMORY_%d", time.Now().UnixNano())

	cmd := fmt.Sprintf(`mkdir -p %s && cat <<'%s' > %s/%s
%s
%s`,
		memoryDir,
		delimiter,
		memoryDir,
		filename,
		strings.TrimSpace(fileContent),
		delimiter,
	)

	execConfig := container.ExecOptions{
		Cmd:          []string{"bash", "-lc", cmd},
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := m.client.ContainerExecCreate(ctx, containerName, execConfig)
	if err != nil {
		return fmt.Errorf("create memory write exec for %s: %w", containerName, err)
	}

	if err := m.client.ContainerExecStart(ctx, execID.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("start memory write exec for %s: %w", containerName, err)
	}

	inspect, err := m.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return fmt.Errorf("inspect memory write exec for %s: %w", containerName, err)
	}
	if inspect.ExitCode != 0 {
		return fmt.Errorf("memory write exec for %s exited %d", containerName, inspect.ExitCode)
	}

	return nil
}
