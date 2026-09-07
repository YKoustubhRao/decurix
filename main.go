package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"sort"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/joho/godotenv"
)

const (
	listenAddr    = ":8080"
	imageName     = "flowera-desktop:latest"
	containerPort = "6901/tcp"

	firstDesktopPort = 6080
	maxDesktopPort   = 65000

	agentPortOffset = 10000
)

type Desktop struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContainerID string `json:"container_id"`
	Volume      string `json:"volume"`
	Port        int    `json:"port"`
	Status      string `json:"status"`
	URL         string `json:"url"`
	CreatedAt   string `json:"created_at"`
}

type HelpNote struct {
	ID          string    `json:"id"`
	DesktopID   string    `json:"desktop_id"`
	VMName      string    `json:"vm_name"`
	Filename    string    `json:"filename"`
	Content     string    `json:"content"`
	CreatedAt   time.Time `json:"created_at"`
}

type Manager struct {
	client *client.Client

	mu sync.Mutex

	helpNotes map[string]HelpNote
}

type PageData struct {
	Desktops []Desktop
	Host     string
}

var (
	manager *Manager
	tmpl    *template.Template
)

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
	if err := manager.ensureImage(ctx); err != nil {
		log.Printf("warning: could not ensure image: %v", err)
	}

	templatePath := filepath.Join("templates", "index.html")

	tmpl = template.Must(
		template.ParseFiles(templatePath),
	)

	http.HandleFunc("/", dashboardHandler)
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

func desktopsAPIHandler(w http.ResponseWriter, r *http.Request) {
	desktops, err := manager.listDesktops(r.Context())
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, desktops)
}

func createDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))

	if name == "" {
		name = fmt.Sprintf("desktop-%d", time.Now().Unix())
	}

	desktop, err := manager.createDesktop(
		r.Context(),
		name,
	)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, desktop)
}

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

func (m *Manager) ensureImage(ctx context.Context) error {
	_, _, err := m.client.ImageInspectWithRaw(ctx, imageName)

	if err == nil {
		return nil
	}

	log.Printf("Pulling %s...", imageName)

	reader, err := m.client.ImagePull(
		ctx,
		imageName,
		image.PullOptions{},
	)
	if err != nil {
		return err
	}

	defer reader.Close()

	// Consume the pull output.
	buf := make([]byte, 4096)

	for {
		_, err := reader.Read(buf)
		if err != nil {
			break
		}
	}

	log.Println("Image ready.")

	return nil
}

func (m *Manager) createDesktop(
	ctx context.Context,
	name string,
) (*Desktop, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	// Find an available host port.
	port, err := m.findFreePort(ctx)
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

	defaultPassword := os.Getenv("DEFAULT_PASSWORD")
	if defaultPassword == "" {
		defaultPassword = "admin"
	}
	env := []string{
		"VNC_PW=" + defaultPassword,
		"VNC_RESOLUTION=1280x720",
		"DESKTOP_ID=" + strings.ToLower(strings.ReplaceAll(name, " ", "-")),
		"DESKTOP_NAME=" + name,
		"CONTROL_PLANE_URL=http://host.docker.internal:8080",
	}

	hostPort := strconv.Itoa(port)
	agentHostPort := strconv.Itoa(port + agentPortOffset)

	config := &container.Config{
		Image: imageName,
		Env:   env,

		Hostname: containerName,

		Labels: map[string]string{
			"desktop-manager": "true",
		},
	}

	hostConfig := &container.HostConfig{
		ShmSize: 512 * 1024 * 1024,

		PortBindings: nat.PortMap{
			nat.Port(containerPort): []nat.PortBinding{
				{
					HostIP:   "0.0.0.0",
					HostPort: hostPort,
				},
			},

			nat.Port("8081/tcp"): []nat.PortBinding{
				{
					HostIP:   "0.0.0.0",
					HostPort: agentHostPort,
				},
			},
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

	networkingConfig := (*network.NetworkingConfig)(nil)

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
		ContainerID: resp.ID,
		Volume:      volumeName,
		Port:        port,
		Status:      "running",
		URL: fmt.Sprintf(
			"http://%s:%d/vnc.html",
			getServerHost(),
			port,
		),
		CreatedAt: time.Now().Format(time.RFC3339),
	}, nil
}

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

		port := 0

		for _, p := range c.Ports {
			if p.PrivatePort == 6901 &&
				p.Type == "tcp" &&
				p.PublicPort > 0 {

				port = int(p.PublicPort)
				break
			}
		}

		volumeName := ""

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

		url := ""

		if port > 0 {
			url = fmt.Sprintf(
				"http://%s:%d/vnc.html",
				getServerHost(),
				port,
			)
		}

		result = append(
			result,
			Desktop{
				ID:          id,
				Name:        id,
				ContainerID: c.ID,
				Volume:      volumeName,
				Port:        port,
				Status:      status,
				URL:         url,
				CreatedAt:   createdAt,
			},
		)
	}

	return result, nil
}

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
// Port allocation
// ------------------------------------------------------------

func (m *Manager) findFreePort(
	ctx context.Context,
) (int, error) {

	containers, err := m.client.ContainerList(
		ctx,
		container.ListOptions{
			All: true,
		},
	)
	if err != nil {
		return 0, err
	}

	used := make(map[int]bool)

	for _, c := range containers {
		for _, p := range c.Ports {
			if p.Type == "tcp" &&
				p.PublicPort > 0 {

				used[int(p.PublicPort)] = true
			}
		}
	}

	for port := firstDesktopPort; port <= maxDesktopPort; port++ {
		if !used[port] {
			return port, nil
		}
	}

	return 0, fmt.Errorf(
		"no free desktop ports",
	)
}

// ------------------------------------------------------------
// Helpers
// ------------------------------------------------------------

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

func getServerHost() string {
	host := os.Getenv("DESKTOP_HOST")

	if host != "" {
		return host
	}

	return "localhost"
}

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

func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	_ = json.NewEncoder(w).Encode(v)
}

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

func (m *Manager) sendMessage(
    ctx context.Context,
    id string,
    content string,
) error {

    desktopID := sanitizeName(id)

    name := "desktop_" + desktopID

    inspect, err := m.client.ContainerInspect(
        ctx,
        name,
    )
    if err != nil {
        return fmt.Errorf("find desktop: %w", err)
    }

    agentPort := 16080

    for _, binding := range inspect.NetworkSettings.Ports["8081/tcp"] {
        if binding.HostPort != "" {
            p, err := strconv.Atoi(binding.HostPort)
            if err == nil {
                agentPort = p
                break
            }
        }
    }

    payload := map[string]string{
        "content": content,
    }

    body, err := json.Marshal(payload)
    if err != nil {
        return err
    }

    url := fmt.Sprintf(
        "http://host.docker.internal:%d/message",
        agentPort,
    )

    req, err := http.NewRequestWithContext(
        ctx,
        http.MethodPost,
        url,
        strings.NewReader(string(body)),
    )
    if err != nil {
        return err
    }

    req.Header.Set(
        "Content-Type",
        "application/json",
    )

    client := &http.Client{
        Timeout: 10 * time.Second,
    }

    resp, err := client.Do(req)
    if err != nil {
        return fmt.Errorf(
            "send message to desktop: %w",
            err,
        )
    }

    defer resp.Body.Close()

    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return fmt.Errorf(
            "desktop returned HTTP %d",
            resp.StatusCode,
        )
    }

    return nil
}

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

func helpNotesHandler(w http.ResponseWriter, r *http.Request) {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	notes := make([]HelpNote, 0, len(manager.helpNotes))

	for _, note := range manager.helpNotes {
		notes = append(notes, note)
	}

	jsonResponse(w, notes)
}

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
