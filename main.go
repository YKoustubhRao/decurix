package main

import (
	"regexp"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"strconv"
	"sync"
	"time"
	"sort"
	"net/url"
	"net/http/httputil"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/joho/godotenv"
)

const (
	listenAddr    = ":8080"
	containerPort = "6901/tcp"

	desktopImageBasic     = "flowera-desktop:basic"
	desktopImageDeveloper = "flowera-desktop:developer"
	desktopImageAI        = "flowera-desktop:ai"


	ollamaContainerName = "flowera-ollama"
	ollamaImage         = "ollama/ollama:latest"
	ollamaPort          = "11434/tcp"
	ollamaModel         = "qwen3:0.6b"

	floweraNetwork = "flowera-network"
	ollamaVolume   = "flowera_ollama"
)

// Desktop represents a managed desktop container: its identity, profile, backing Docker resources, status, and resource allocation.
type Desktop struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Profile     string `json:"profile"`
	ContainerID string `json:"container_id"`
	Volume      string `json:"volume"`
	Status      string `json:"status"`
	URL         string `json:"url"`
	CreatedAt   string `json:"created_at"`

	CPUs        int64 `json:"cpus"`
	MemoryMB    int64 `json:"memory_mb"`
}

// HelpNote is a support note submitted from inside a running desktop, tied to a specific desktop and VM.
type HelpNote struct {
	ID          string    `json:"id"`
	DesktopID   string    `json:"desktop_id"`
	VMName      string    `json:"vm_name"`
	Filename    string    `json:"filename"`
	Content     string    `json:"content"`
	CreatedAt   time.Time `json:"created_at"`
}

// Manager owns the Docker client and the in-memory set of help notes, guarded by a mutex for concurrent access.
type Manager struct {
	client *client.Client

	mu sync.Mutex

	helpNotes map[string]HelpNote
}

// PageData is the template data passed to the dashboard page: the current desktops and the request host.
type PageData struct {
	Desktops []Desktop
	Host     string
}

var (
	manager *Manager
	tmpl    *template.Template
)

// main loads configuration, sets up the Docker client, ensures required images/networks exist, registers HTTP routes, and starts the server.
func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found; using environment variables")
		os.Exit(1)
	}

	ctx := context.Background()

	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close()

	manager = &Manager{
		client: cli,
		helpNotes: make(map[string]HelpNote),
	}

	// Pull the desktop image if it isn't already present.
	if err := manager.ensureImages(ctx); err != nil {
		log.Printf("warning: could not ensure image: %v", err)
	}

	if err := manager.ensureFloweraNetwork(ctx); err != nil {
		log.Fatal(err)
	}

	if err := manager.ensureOllama(ctx); err != nil {
		log.Printf("warning: Ollama unavailable: %v", err)
	}

	templatePath := filepath.Join("templates", "index.html")

	tmpl = template.Must(
		template.ParseFiles(templatePath),
	)

	http.HandleFunc("/", dashboardHandler)
	http.HandleFunc("/desktop/", desktopProxyHandler)
	http.HandleFunc("/api/desktops", desktopsAPIHandler)
	http.HandleFunc("/api/desktops/create", createDesktopHandler)
	http.HandleFunc("/api/desktops/start", startDesktopHandler)
	http.HandleFunc("/api/desktops/stop", stopDesktopHandler)
	http.HandleFunc("/api/desktops/restart", restartDesktopHandler)
	http.HandleFunc("/api/desktops/delete", deleteDesktopHandler)
	http.HandleFunc("/messages", messagesDashboardHandler)
	http.HandleFunc("/api/desktops/send", sendMessageHandler)
	http.HandleFunc("/api/helptext", helpTextHandler)
	http.HandleFunc("/api/helpnotes", helpNotesHandler)
	http.HandleFunc("/api/helpnotes/close", closeHelpNoteHandler)
	http.HandleFunc("/dashboard", dashboardPostsHandler)


	log.Printf("Desktop manager listening on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}

// ------------------------------------------------------------
// Dashboard
// ------------------------------------------------------------

// dashboardHandler renders the root dashboard page listing all desktops.
func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	desktops, err := manager.listDesktops(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	host := getHost(r)

	data := PageData{
		Desktops: desktops,
		Host:     host,
	}

	if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ------------------------------------------------------------
// API
// ------------------------------------------------------------

// desktopsAPIHandler returns the current list of desktops as JSON.
func desktopsAPIHandler(w http.ResponseWriter, r *http.Request) {
	desktops, err := manager.listDesktops(r.Context())
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, desktops)
}

// createDesktopHandler validates a desktop-creation request (name, profile, cpus, memory) and creates the desktop.
func createDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	profile := strings.TrimSpace(r.FormValue("profile"))

	cpus := int64(2)
	memoryMB := int64(4096)

	if profile == "" {
		profile = "basic"
	}

	switch profile {
	case "basic", "developer", "ai":
		// valid
	default:
		http.Error(
			w,
			"invalid profile: must be basic, developer, or ai",
			http.StatusBadRequest,
		)
		return
	}

	if v := r.FormValue("cpus"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			cpus = parsed
		}
	}

	if v := r.FormValue("memory_mb"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			memoryMB = parsed
		}
	}

	if name == "" {
		name = fmt.Sprintf("desktop-%d", time.Now().Unix())
	}

	validNameRe := regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	if !validNameRe.MatchString(name) {
		http.Error(w, "invalid name: only alphanumeric, hyphens, underscores, and dots allowed", http.StatusBadRequest)
		return
	}

	if cpus <= 0 {
		http.Error(w, "cpus must be positive", http.StatusBadRequest)
		return
	}

	if cpus > 64 {
		http.Error(w, "cpus must not exceed 64", http.StatusBadRequest)
		return
	}

	if memoryMB <= 0 {
		http.Error(w, "memory_mb must be positive", http.StatusBadRequest)
		return
	}

	if memoryMB > 65536 {
		http.Error(w, "memory_mb must not exceed 65536", http.StatusBadRequest)
		return
	}

	desktop, err := manager.createDesktop(
		r.Context(),
		name,
		profile,
		cpus,
		memoryMB,
	)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, desktop)
}

// startDesktopHandler starts an existing desktop by ID.
func startDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := manager.startDesktop(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "started",
	})
}

// stopDesktopHandler stops a running desktop by ID.
func stopDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := manager.stopDesktop(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "stopped",
	})
}

// restartDesktopHandler restarts a desktop by ID.
func restartDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := manager.restartDesktop(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "restarted",
	})
}

// deleteDesktopHandler deletes a desktop and its persistent storage by ID.
func deleteDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := manager.deleteDesktop(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "deleted",
	})
}

// ------------------------------------------------------------
// Docker
// ------------------------------------------------------------

// ensureImages pulls any required desktop Docker images that are not already present locally.
func (m *Manager) ensureImages(ctx context.Context) error {
	images := []string{
		desktopImageBasic,
		desktopImageDeveloper,
		desktopImageAI,
	}

	for _, imageName := range images {
		_, _, err := m.client.ImageInspectWithRaw(
			ctx,
			imageName,
		)

		if err == nil {
			log.Printf("Image %s already exists.", imageName)
			continue
		}

		log.Printf("Pulling %s...", imageName)

		reader, err := m.client.ImagePull(
			ctx,
			imageName,
			image.PullOptions{},
		)
		if err != nil {
			return fmt.Errorf(
				"pull %s: %w",
				imageName,
				err,
			)
		}

		buf := make([]byte, 4096)

		for {
			_, err := reader.Read(buf)
			if err != nil {
				break
			}
		}

		reader.Close()

		log.Printf("Image %s ready.", imageName)
	}

	return nil
}

// createDesktop provisions a new desktop container (volume, env, container config) for the given profile and starts it, rolling back on failure.
func (m *Manager) createDesktop(
	ctx context.Context,
	name string,
	profile string,
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

	env := buildDesktopEnv(name)

	config, hostConfig, networkingConfig := buildContainerConfigs(
		imageName,
		containerName,
		profile,
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
		ContainerID: resp.ID,
		Volume:      volumeName,
		Status:      "running",
		CPUs:        cpus,
		MemoryMB:    memoryMB,
		URL: fmt.Sprintf(
			"http://%s:8080/desktop/%s/vnc.html",
			getServerHost(),
			sanitizeName(name),
		),
		CreatedAt: time.Now().Format(time.RFC3339),
	}, nil
}

// resolveDesktopImage maps a desktop profile (basic/developer/ai) to its corresponding Docker image, or returns an error for an unknown profile.
func resolveDesktopImage(profile string) (string, error) {
	switch profile {
	case "basic":
		return desktopImageBasic, nil
	case "developer":
		return desktopImageDeveloper, nil
	case "ai":
		return desktopImageAI, nil
	default:
		return "", fmt.Errorf(
			"invalid desktop profile: %s",
			profile,
		)
	}
}

// buildDesktopEnv builds the container environment variables (VNC, desktop identity, control-plane/Ollama URLs) for a desktop by name.
func buildDesktopEnv(name string) []string {
	defaultPassword := os.Getenv("DEFAULT_PASSWORD")
	if defaultPassword == "" {
		defaultPassword = "admin"
	}
	return []string{
		"VNC_PW=" + defaultPassword,
		"VNC_RESOLUTION=1280x720",
		"DESKTOP_ID=" + strings.ToLower(
			strings.ReplaceAll(name, " ", "-"),
		),
		"DESKTOP_NAME=" + name,
		"CONTROL_PLANE_URL=http://host.docker.internal:8080",
		"OLLAMA_URL=http://flowera-ollama:11434",
		"OLLAMA_MODEL=" + ollamaModel,
	}
}

// buildContainerConfigs builds the Docker container, host, and network configuration for a desktop from its resolved image, resources, and profile.
func buildContainerConfigs(imageName, containerName, profile string, cpus, memoryMB int64, env []string, volumeName string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	config := &container.Config{
		Image: imageName,
		Env:   env,
		Hostname: containerName,
		Labels: map[string]string{
			"desktop-manager": "true",
			"desktop-profile": profile,
		},
	}

	hostConfig := &container.HostConfig{
		ShmSize: 512 * 1024 * 1024,
		Resources: container.Resources{
			NanoCPUs: cpus * 1_000_000_000,
			Memory:   memoryMB * 1024 * 1024,
		},
		Binds: []string{
			volumeName + ":/home/headless",
		},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
		ExtraHosts: []string{
			"host.docker.internal:host-gateway",
		},
	}

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			floweraNetwork: {},
		},
	}

	return config, hostConfig, networkingConfig
}

// listDesktops returns all desktop containers managed by this instance, read from their Docker labels and inspect data.
func (m *Manager) listDesktops(
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
			getServerHost(),
			id,
		)

		result = append(
			result,
			Desktop{
				ID:          id,
				Name:        id,
				Profile:     profile,
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

// startDesktop starts the Docker container backing the given desktop ID.
func (m *Manager) startDesktop(
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

// stopDesktop stops the Docker container backing the given desktop ID.
func (m *Manager) stopDesktop(
	ctx context.Context,
	id string,
) error {

	name := "desktop_" + sanitizeName(id)

	timeout := 10

	return m.client.ContainerStop(
		ctx,
		name,
		container.StopOptions{
			Timeout: &timeout,
		},
	)
}

// restartDesktop stops and restarts the Docker container backing the given desktop ID.
func (m *Manager) restartDesktop(
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

// deleteDesktop removes the Docker container and persistent volume backing the given desktop ID.
func (m *Manager) deleteDesktop(
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



// ------------------------------------------------------------
// Helpers
// ------------------------------------------------------------

// sanitizeName normalizes a string into a safe lowercase container/volume name fragment.
func sanitizeName(s string) string {
	s = strings.ToLower(s)

	var b strings.Builder

	for _, r := range s {
		if (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '-' ||
			r == '_' {

			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}

	return strings.Trim(
		b.String(),
		"-_",
	)
}

// getServerHost returns the configured external host for this server, defaulting to localhost.
func getServerHost() string {
	host := os.Getenv("DESKTOP_HOST")

	if host != "" {
		return host
	}

	return "localhost"
}

// getHost derives the externally-reachable host from an incoming request, substituting the configured desktop host for localhost.
func getHost(r *http.Request) string {
	host := r.Host

	if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}

	if host == "localhost" ||
		host == "127.0.0.1" {

		envHost := os.Getenv("DESKTOP_HOST")

		if envHost != "" {
			return envHost
		}
	}

	return host
}

// jsonResponse writes a value to the response writer as JSON.
func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	_ = json.NewEncoder(w).Encode(v)
}

// jsonError writes an error to the response writer as a JSON-formatted HTTP 500 response.
func jsonError(w http.ResponseWriter, err error) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	w.WriteHeader(http.StatusInternalServerError)

	_ = json.NewEncoder(w).Encode(
		map[string]string{
			"error": err.Error(),
		},
	)
}

// messagesDashboardHandler renders the messages dashboard page for sending messages to desktops.
func messagesDashboardHandler(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path != "/messages" {
        http.NotFound(w, r)
        return
    }

    desktops, err := manager.listDesktops(r.Context())
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    data := PageData{
        Desktops: desktops,
        Host:     getHost(r),
    }

    messageTmpl := template.Must(
        template.ParseFiles(
            filepath.Join("templates", "messages.html"),
        ),
    )

    if err := messageTmpl.ExecuteTemplate(
        w,
        "messages.html",
        data,
    ); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
    }
}

// sendMessageHandler validates and forwards a message-send request to a specific desktop.
func sendMessageHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "POST required", http.StatusMethodNotAllowed)
        return
    }

    desktopID := strings.TrimSpace(r.FormValue("id"))
    content := r.FormValue("content")

    if desktopID == "" {
        http.Error(w, "missing desktop id", http.StatusBadRequest)
        return
    }

    if content == "" {
        http.Error(w, "empty message", http.StatusBadRequest)
        return
    }

    if len(content) > 1000 {
        http.Error(w, "message too long", http.StatusBadRequest)
        return
    }

    if err := manager.sendMessage(
        r.Context(),
        desktopID,
        content,
    ); err != nil {
        jsonError(w, err)
        return
    }

    jsonResponse(w, map[string]string{
        "status": "sent",
    })
}

// sendMessage posts a JSON message to a running desktop's internal message endpoint.
func (m *Manager) sendMessage(
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

// helpTextHandler records a help note submitted from inside a desktop.
func helpTextHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DesktopID string `json:"desktop_id"`
		VMName    string `json:"vm_name"`
		Filename  string `json:"filename"`
		Content   string `json:"content"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	note := HelpNote{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		DesktopID: req.DesktopID,
		VMName:    req.VMName,
		Filename:  req.Filename,
		Content:   req.Content,
		CreatedAt: time.Now(),
	}

	manager.mu.Lock()
	manager.helpNotes[note.ID] = note
	manager.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]string{
		"status": "received",
	})
}

// helpNotesHandler returns all recorded help notes as JSON.
func helpNotesHandler(w http.ResponseWriter, r *http.Request) {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	notes := make([]HelpNote, 0, len(manager.helpNotes))

	for _, note := range manager.helpNotes {
		notes = append(notes, note)
	}

	jsonResponse(w, notes)
}

// closeHelpNoteHandler deletes a help note by ID.
func closeHelpNoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	manager.mu.Lock()
	delete(manager.helpNotes, id)
	manager.mu.Unlock()

	jsonResponse(w, map[string]string{
		"status": "closed",
	})
}

// dashboardPostsHandler renders the help-notes dashboard, most recent first.
func dashboardPostsHandler(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path != "/dashboard" {
        http.NotFound(w, r)
        return
    }

    manager.mu.Lock()

    notes := make([]HelpNote, 0, len(manager.helpNotes))

    for _, note := range manager.helpNotes {
        notes = append(notes, note)
    }

    manager.mu.Unlock()

    sort.Slice(notes, func(i, j int) bool {
        return notes[i].CreatedAt.After(notes[j].CreatedAt)
    })

    data := struct {
        Notes []HelpNote
    }{
        Notes: notes,
    }

    dashboardTmpl := template.Must(
        template.ParseFiles(
            filepath.Join("templates", "dashboard.html"),
        ),
    )

    if err := dashboardTmpl.Execute(w, data); err != nil {
        http.Error(
            w,
            err.Error(),
            http.StatusInternalServerError,
        )
        return
    }
}


// ensureFloweraNetwork creates the shared Docker network used by desktops if it does not already exist.
func (m *Manager) ensureFloweraNetwork(ctx context.Context) error {
	_, err := m.client.NetworkInspect(
		ctx,
		floweraNetwork,
		network.InspectOptions{},
	)

	if err == nil {
		return nil
	}

	log.Printf("Creating Docker network %s...", floweraNetwork)

	_, err = m.client.NetworkCreate(
		ctx,
		floweraNetwork,
		network.CreateOptions{
			Driver: "bridge",
			Labels: map[string]string{
				"flowera": "true",
			},
		},
	)

	if err != nil {
		return fmt.Errorf(
			"create Flowera network: %w",
			err,
		)
	}

	return nil
}

// ensureOllama provisions and starts the shared Ollama container and waits for it to become ready.
func (m *Manager) ensureOllama(ctx context.Context) error {
	// ------------------------------------------------------------
	// Make sure Ollama image exists
	// ------------------------------------------------------------

	_, _, err := m.client.ImageInspectWithRaw(
		ctx,
		ollamaImage,
	)

	if err != nil {
		log.Printf("Pulling %s...", ollamaImage)

		reader, err := m.client.ImagePull(
			ctx,
			ollamaImage,
			image.PullOptions{},
		)

		if err != nil {
			return fmt.Errorf(
				"pull Ollama image: %w",
				err,
			)
		}

		defer reader.Close()

		buf := make([]byte, 4096)

		for {
			_, err := reader.Read(buf)

			if err != nil {
				break
			}
		}
	}

	// ------------------------------------------------------------
	// Create persistent volume
	// ------------------------------------------------------------

	_, err = m.client.VolumeCreate(
		ctx,
		volume.CreateOptions{
			Name: ollamaVolume,
		},
	)

	if err != nil {
		// Volume may already exist.
		log.Printf(
			"Ollama volume already exists or could not be created: %v",
			err,
		)
	}

	// ------------------------------------------------------------
	// Check whether container already exists
	// ------------------------------------------------------------

	inspect, err := m.client.ContainerInspect(
		ctx,
		ollamaContainerName,
	)

	if err == nil {

		// Container exists.

		if inspect.State != nil &&
			inspect.State.Running {

			log.Println("Ollama container already running.")

		} else {

			log.Println("Starting existing Ollama container...")

			if err := m.client.ContainerStart(
				ctx,
				ollamaContainerName,
				container.StartOptions{},
			); err != nil {

				return fmt.Errorf(
					"start Ollama: %w",
					err,
				)
			}
		}

	} else {

		// --------------------------------------------------------
		// Create Ollama container
		// --------------------------------------------------------

		log.Println("Creating Ollama container...")

		config := &container.Config{
			Image: ollamaImage,

			Env: []string{
				"OLLAMA_HOST=0.0.0.0:11434",
			},

			Labels: map[string]string{
				"flowera":       "true",
				"flowera-ollama": "true",
			},
		}

		hostConfig := &container.HostConfig{
			Binds: []string{
				ollamaVolume + ":/root/.ollama",
			},

			RestartPolicy: container.RestartPolicy{
				Name: "unless-stopped",
			},
		}

		networkingConfig := &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				floweraNetwork: {},
			},
		}

		resp, err := m.client.ContainerCreate(
			ctx,
			config,
			hostConfig,
			networkingConfig,
			nil,
			ollamaContainerName,
		)

		if err != nil {
			return fmt.Errorf(
				"create Ollama container: %w",
				err,
			)
		}

		if err := m.client.ContainerStart(
			ctx,
			resp.ID,
			container.StartOptions{},
		); err != nil {

			return fmt.Errorf(
				"start Ollama container: %w",
				err,
			)
		}
	}

	// ------------------------------------------------------------
	// Wait for Ollama API
	// ------------------------------------------------------------

	log.Println("Waiting for Ollama...")

	if err := waitForOllama(ctx); err != nil {
		return err
	}

	// ------------------------------------------------------------
	// Pull model
	// ------------------------------------------------------------

	log.Printf(
		"Ensuring Ollama model %s...",
		ollamaModel,
	)

	if err := m.ensureOllamaModel(ctx); err != nil {
		return err
	}

	log.Println("Ollama ready.")

	return nil
}

// waitForOllama polls the Ollama API until it responds successfully, the context is cancelled, or a two-minute deadline elapses.
func waitForOllama(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)

	for time.Now().Before(deadline) {

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			"http://host.docker.internal:11434/api/tags",
			nil,
		)

		if err == nil {

			client := &http.Client{
				Timeout: 3 * time.Second,
			}

			resp, err := client.Do(req)

			if err == nil {

				resp.Body.Close()

				if resp.StatusCode >= 200 &&
					resp.StatusCode < 300 {

					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return fmt.Errorf(
		"timeout waiting for Ollama",
	)
}

// ensureOllamaModel pulls the configured Ollama model into the shared Ollama container if it is not already present.
func (m *Manager) ensureOllamaModel(ctx context.Context) error {
	// Try `ollama show MODEL`.
	// Exit code 0 means the model already exists.
	checkExec, err := m.client.ContainerExecCreate(
		ctx,
		ollamaContainerName,
		container.ExecOptions{
			Cmd: []string{
				"ollama",
				"show",
				ollamaModel,
			},
			AttachStdout: true,
			AttachStderr: true,
		},
	)

	if err != nil {
		return fmt.Errorf(
			"create Ollama model check: %w",
			err,
		)
	}

	if err := m.client.ContainerExecStart(
		ctx,
		checkExec.ID,
		container.ExecStartOptions{},
	); err != nil {
		return fmt.Errorf(
			"start Ollama model check: %w",
			err,
		)
	}

	for {
		result, err := m.client.ContainerExecInspect(
			ctx,
			checkExec.ID,
		)

		if err != nil {
			return fmt.Errorf(
				"inspect Ollama model check: %w",
				err,
			)
		}

		if !result.Running {
			if result.ExitCode == 0 {
				log.Printf(
					"Ollama model %s already exists.",
					ollamaModel,
				)

				return nil
			}

			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-time.After(200 * time.Millisecond):
		}
	}

	// Model doesn't exist. Pull it.
	log.Printf(
		"Pulling Ollama model %s...",
		ollamaModel,
	)

	pullExec, err := m.client.ContainerExecCreate(
		ctx,
		ollamaContainerName,
		container.ExecOptions{
			Cmd: []string{
				"ollama",
				"pull",
				ollamaModel,
			},
			AttachStdout: true,
			AttachStderr: true,
		},
	)

	if err != nil {
		return fmt.Errorf(
			"create Ollama pull: %w",
			err,
		)
	}

	if err := m.client.ContainerExecStart(
		ctx,
		pullExec.ID,
		container.ExecStartOptions{},
	); err != nil {
		return fmt.Errorf(
			"start Ollama pull: %w",
			err,
		)
	}

	for {
		result, err := m.client.ContainerExecInspect(
			ctx,
			pullExec.ID,
		)

		if err != nil {
			return fmt.Errorf(
				"inspect Ollama pull: %w",
				err,
			)
		}

		if !result.Running {
			if result.ExitCode != 0 {
				return fmt.Errorf(
					"Ollama pull failed with exit code %d",
					result.ExitCode,
				)
			}

			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-time.After(1 * time.Second):
		}
	}

	log.Printf(
		"Ollama model %s ready.",
		ollamaModel,
	)

	return nil
}

// desktopProxyHandler reverse-proxies HTTP requests to the desktop container's internal address.
func desktopProxyHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(
		strings.TrimPrefix(r.URL.Path, "/desktop/"),
		"/",
		2,
	)

	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	desktopID := sanitizeName(parts[0])
	name := "desktop_" + desktopID

	inspect, err := manager.client.ContainerInspect(
		r.Context(),
		name,
	)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if inspect.State == nil || !inspect.State.Running {
		http.Error(
			w,
			"desktop is not running",
			http.StatusServiceUnavailable,
		)
		return
	}

	networkInfo, ok := inspect.NetworkSettings.Networks[floweraNetwork]
	if !ok || networkInfo == nil {
		http.Error(
			w,
			"desktop is not connected to Flowera network",
			http.StatusServiceUnavailable,
		)
		return
	}

	ip := networkInfo.IPAddress

	if ip == "" {
		http.Error(
			w,
			"desktop has no network IP",
			http.StatusServiceUnavailable,
		)
		return
	}

	target := fmt.Sprintf(
		"http://%s:%s",
		ip,
		strings.TrimSuffix(containerPort, "/tcp"),
	)

	proxyURL, err := url.Parse(target)
	if err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)

	// Remove /desktop/<id> from the URL.
	prefix := "/desktop/" + desktopID

	r.URL.Path = strings.TrimPrefix(
		r.URL.Path,
		prefix,
	)

	if r.URL.Path == "" || r.URL.Path == "/" {
		r.URL.Path = "/vnc.html"
	}

	// Preserve query parameters.
	reverseProxy.ErrorHandler = func(
		rw http.ResponseWriter,
		req *http.Request,
		err error,
	) {
		log.Printf(
			"desktop proxy error for %s: %v",
			desktopID,
			err,
		)

		http.Error(
			rw,
			"desktop unavailable",
			http.StatusBadGateway,
		)
	}

	reverseProxy.ServeHTTP(w, r)
}