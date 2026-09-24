package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
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

	// HostGUISecret signs the launch tokens written into launchers for Windows apps that run on the host (see
	// internal/winhost); set by main before any desktop is created.
	HostGUISecret []byte

	// listMu guards the List result cache: one Docker round trip answers every
	// List call inside listCacheTTL, and concurrent callers share the one in
	// flight. Measured: 20 dashboards polling /api/desktops at once made the
	// tail 31 s with ten timeouts on a host under memory pressure, all for a
	// list of two desktops. listGen is bumped by every lifecycle change so a
	// result computed before it is never cached over it.
	listMu     sync.Mutex
	listAt     time.Time
	listVal    []Desktop
	listGen    uint64
	listFlight *listCall

	// shots caches the dashboard's desktop thumbnails, keyed by desktop ID.
	// Guarded by shotMu, which covers the map alone - each entry carries its
	// own lock for the capture itself (see screenshot.go), so one slow
	// desktop never stalls another's thumbnail.
	shotMu sync.Mutex
	shots  map[string]*shotEntry

	// claudeLogin is the one in-progress "claude auth login" attempt (see
	// claudelogin.go) - there is one shared Claude session for the whole
	// app, so only one login attempt is ever in flight. Guarded by its own
	// mutex, separate from shotMu/viewedMu since a login attempt has
	// nothing to do with either of those.
	claudeLoginMu sync.Mutex
	claudeLogin   *claudeLoginSession

	// viewed is the last time each desktop's thumbnail was fetched or its
	// screen was actually proxied to, keyed by sanitized desktop ID. Guarded
	// by its own mutex for the same reason shots is. internal/idlepause
	// reads it so a desktop nobody has a task running on, but someone still
	// has open in a browser tab, is never stopped out from under them.
	viewedMu sync.Mutex
	viewed   map[string]time.Time

	// secOpt/secOptResolved cache whether the Docker daemon enforces
	// seccomp (see sandbox.go's desktopSecurityOpt) - a property of the
	// daemon that cannot change while it keeps running, so it is worth
	// asking at most once rather than on every desktop Create.
	secOptMu       sync.Mutex
	secOpt         []string
	secOptResolved bool

	// toolsMu/toolsPopulated ensure the shared decurix-tools volume is
	// populated at most once in-process, preventing redundant init containers
	// and disk I/O on every VM creation.
	toolsMu        sync.Mutex
	toolsPopulated bool

	// startedMu/startedCache cache Docker's StartedAt timestamp for running
	// containers, eliminating redundant ContainerInspect RPCs during List.
	startedMu    sync.Mutex
	startedCache map[string]string

	// proxyTargetMu and proxyTargets cache resolved reverse-proxy URLs for running
	// desktop containers (VNC port and agent port). Eliminates dozens of
	// blocking Docker ContainerInspect calls when noVNC loads ~40 static assets.
	proxyTargetMu sync.RWMutex
	proxyTargets  map[string]proxyTargetCacheEntry

	// resourceMu/resourceOverrides track dynamically updated CPU and memory
	// allocations for containers, keeping Desktop.CPUs and Desktop.MemoryMB in
	// sync even though Docker labels cannot be altered after container creation.
	resourceMu        sync.Mutex
	resourceOverrides map[string]ResourceAllocation

	// httpClient pools persistent HTTP connections (Keep-Alive) to desktop
	// agent APIs on port 8081, eliminating TCP handshake overhead on every
	// command dispatch.
	httpClient *http.Client

	// provisioningMu and provisioning track in-flight desktop creation so that
	// requests are accepted immediately (< 15ms) while the container spins up
	// and desktop icons/apps are migrated in the background.
	provisioningMu sync.RWMutex
	provisioning   map[string]*ProvisioningDesktop
}

// proxyTargetCacheEntry stores a cached reverse-proxy URL along with its expiration.
type proxyTargetCacheEntry struct {
	proxyURL *url.URL
	expires  time.Time
}

// InvalidateProxyTarget purges any cached reverse-proxy URLs for the given desktop ID.
func (m *Manager) InvalidateProxyTarget(rawID string) {
	sanitizedID := sanitizeName(rawID)
	m.proxyTargetMu.Lock()
	defer m.proxyTargetMu.Unlock()
	for k := range m.proxyTargets {
		if strings.HasPrefix(k, sanitizedID+":") {
			delete(m.proxyTargets, k)
		}
	}
}

// NewManager creates a Manager backed by the given Docker client.
func NewManager(cli *client.Client) *Manager {
	return &Manager{
		client:            cli,
		startedCache:      make(map[string]string),
		proxyTargets:      make(map[string]proxyTargetCacheEntry),
		resourceOverrides: make(map[string]ResourceAllocation),
		provisioning:      make(map[string]*ProvisioningDesktop),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Create provisions a new desktop container (volume, env, container config)
// for the given profile and starts it, rolling back on failure.
// claudeCredentialsJSON seeds a claude-harness desktop with the shared
// Claude session (see internal/claudeauth); pass "" for every other harness,
// or when nobody has logged in yet. specialization and parentID are "" for
// an ordinary, standalone desktop; see httpapi.createDesktopHandler for how
// a specialized child desktop is spawned from an existing one.
func (m *Manager) Create(
	ctx context.Context,
	name string,
	profile string,
	harness string,
	cpus int64,
	memoryMB int64,
	claudeCredentialsJSON string,
	specialization string,
	parentID string,
	hostApps ...[]string,
) (*Desktop, error) {
	defer m.invalidateList()

	imageName, err := resolveDesktopImage(profile)
	if err != nil {
		return nil, err
	}

	if err := m.EnsureToolsVolume(ctx); err != nil {
		log.Printf("warning: ensure tools volume: %v", err)
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

	env := buildDesktopEnv(name, harness, specialization, parentID)

	config, hostConfig, networkingConfig := buildContainerConfigs(
		imageName,
		containerName,
		profile,
		harness,
		cpus,
		memoryMB,
		env,
		volumeName,
		m.desktopSecurityOpt(ctx),
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

	// Pre-seed host applications onto the volume BEFORE container startup.
	// This ensures desktop launchers, wrapper scripts, credentials, and SVG
	// icons exist on disk before XFCE initializes, avoiding icon refresh lag
	// and startup race conditions.
	if len(hostApps) > 0 && len(hostApps[0]) > 0 {
		if err := m.MigrateHostApps(ctx, sanitizeName(name), hostApps[0]); err != nil {
			log.Printf("hostapps: pre-start migration for %s: %v", containerName, err)
		}
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

	// Best-effort, like PersistMemoryFile: the desktop is still created and
	// usable without it, just unauthenticated until someone logs in and a
	// new desktop picks up the session, or this one is recreated.
	if harness == HarnessClaude && claudeCredentialsJSON != "" {
		if err := m.PersistClaudeCredentials(ctx, sanitizeName(name), claudeCredentialsJSON); err != nil {
			log.Printf("seed claude credentials into %s: %v", containerName, err)
		}
	}

	return &Desktop{
		ID:             sanitizeName(name),
		Name:           name,
		Profile:        profile,
		Harness:        harness,
		Model:          envValue(env, "OLLAMA_MODEL"),
		ContainerID:    resp.ID,
		Volume:         volumeName,
		Status:         "running",
		CPUs:           cpus,
		MemoryMB:       memoryMB,
		URL:            desktopURL(sanitizeName(name)),
		CreatedAt:      time.Now().Format(time.RFC3339),
		Specialization: specialization,
		ParentID:       parentID,
	}, nil
}

// BeginProvisioning registers an in-flight provisioning desktop and launches
// an asynchronous background goroutine to create the volume, container, host app
// migration, container start, and readiness probing. It returns the ProvisioningDesktop
// immediately (< 15ms) so that HTTP APIs and user interfaces never hang.
func (m *Manager) BeginProvisioning(
	name string,
	profile string,
	harness string,
	cpus int64,
	memoryMB int64,
	claudeCredentialsJSON string,
	specialization string,
	parentID string,
	selectedApps []string,
	onStarted func(d *Desktop) error,
) (*ProvisioningDesktop, error) {
	sanitizedID := sanitizeName(name)
	if sanitizedID == "" {
		return nil, fmt.Errorf("invalid desktop name")
	}

	// Verify no existing container or provisioning desktop with this name
	m.provisioningMu.RLock()
	if _, exists := m.provisioning[sanitizedID]; exists {
		m.provisioningMu.RUnlock()
		return nil, fmt.Errorf("desktop %s is already being created", name)
	}
	m.provisioningMu.RUnlock()

	// Check if container already exists
	if _, err := m.client.ContainerInspect(context.Background(), "desktop_"+sanitizedID); err == nil {
		return nil, fmt.Errorf("desktop %s already exists", name)
	}

	prov := &ProvisioningDesktop{
		ID:             sanitizedID,
		Name:           name,
		Profile:        profile,
		Harness:        harness,
		CPUs:           cpus,
		MemoryMB:       memoryMB,
		Status:         "provisioning",
		Stage:          "initializing",
		Message:        "Spinning up desktop...",
		Progress:       10,
		URL:            desktopURL(sanitizedID),
		CreatedAt:      time.Now().Format(time.RFC3339),
		Specialization: specialization,
		ParentID:       parentID,
		Ready:          false,
	}

	// The caller gets a snapshot, never prov itself: runProvisioning keeps updating prov from its own goroutine (under
	// provisioningMu) for the whole creation, and the caller serialises what it is given without holding that lock -
	// returning the live pointer let the two run at once, a data race on every field of the record.
	m.provisioningMu.Lock()
	m.provisioning[sanitizedID] = prov
	snapshot := *prov
	m.provisioningMu.Unlock()

	go m.runProvisioning(prov, claudeCredentialsJSON, selectedApps, onStarted)

	return &snapshot, nil
}

func (m *Manager) runProvisioning(
	prov *ProvisioningDesktop,
	claudeCredentialsJSON string,
	selectedApps []string,
	onStarted func(d *Desktop) error,
) {
	defer m.invalidateList()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sanitizedID := prov.ID
	name := prov.Name
	containerName := "desktop_" + sanitizedID
	volumeName := fmt.Sprintf("desktop_%s_home", sanitizedID)

	updateStage := func(stage, message string, progress int) {
		m.provisioningMu.Lock()
		prov.Stage = stage
		prov.Message = message
		prov.Progress = progress
		m.provisioningMu.Unlock()
	}

	fail := func(err error) {
		log.Printf("desktop provisioning failed for %s: %v", sanitizedID, err)
		m.provisioningMu.Lock()
		prov.Status = "failed"
		prov.Error = err.Error()
		prov.Message = fmt.Sprintf("Creation failed: %v", err)
		m.provisioningMu.Unlock()

		// Clean up Docker resources if partially created
		_ = m.client.ContainerRemove(context.Background(), containerName, container.RemoveOptions{Force: true})
		_ = m.client.VolumeRemove(context.Background(), volumeName, true)

		// Retain error for 60s so UI polls can display the error before cleaning up
		time.AfterFunc(60*time.Second, func() {
			m.provisioningMu.Lock()
			delete(m.provisioning, sanitizedID)
			m.provisioningMu.Unlock()
		})
	}

	// 1. Resolve image
	updateStage("initializing", "Resolving desktop image...", 15)
	imageName, err := resolveDesktopImage(prov.Profile)
	if err != nil {
		fail(err)
		return
	}

	// Prewarm tools volume
	if err := m.EnsureToolsVolume(ctx); err != nil {
		log.Printf("warning: ensure tools volume for %s: %v", sanitizedID, err)
	}

	// 2. Storage Volume
	updateStage("volume", "Creating persistent storage volume...", 30)
	_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName})
	if err != nil {
		fail(fmt.Errorf("create volume: %w", err))
		return
	}

	// 3. Container Configuration & Creation
	updateStage("container", "Creating container environment...", 45)
	env := buildDesktopEnv(name, prov.Harness, prov.Specialization, prov.ParentID)
	prov.Model = envValue(env, "OLLAMA_MODEL")

	config, hostConfig, networkingConfig := buildContainerConfigs(
		imageName,
		containerName,
		prov.Profile,
		prov.Harness,
		prov.CPUs,
		prov.MemoryMB,
		env,
		volumeName,
		m.desktopSecurityOpt(ctx),
	)

	resp, err := m.client.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, containerName)
	if err != nil {
		fail(fmt.Errorf("create container: %w", err))
		return
	}

	// 4. Migrate Desktop Icons & Host Apps
	if len(selectedApps) > 0 {
		updateStage("apps", "Copying desktop icons and host apps...", 65)
		if err := m.MigrateHostApps(ctx, sanitizedID, selectedApps); err != nil {
			log.Printf("hostapps: migration for %s: %v", containerName, err)
		}
	}

	// 5. Start Container
	updateStage("starting", "Starting desktop environment...", 80)
	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		fail(fmt.Errorf("start container: %w", err))
		return
	}

	// Claude session persistence
	if prov.Harness == HarnessClaude && claudeCredentialsJSON != "" {
		if err := m.PersistClaudeCredentials(ctx, sanitizedID, claudeCredentialsJSON); err != nil {
			log.Printf("seed claude credentials into %s: %v", containerName, err)
		}
	}

	// Desktop struct for callbacks
	createdDesktop := prov.ToDesktop()
	createdDesktop.ContainerID = resp.ID
	createdDesktop.Status = "running"

	if onStarted != nil {
		if err := onStarted(&createdDesktop); err != nil {
			log.Printf("desktop onStarted callback for %s: %v", sanitizedID, err)
		}
	}

	// 6. Readiness Probe (Wait for VNC / websockify to listen)
	updateStage("readiness", "Waiting for desktop GUI to initialize...", 90)
	ready := m.WaitForGUIReady(ctx, sanitizedID, 25*time.Second)
	if !ready {
		log.Printf("warning: desktop %s GUI readiness probe timed out (will continue running)", sanitizedID)
	}

	// 7. Ready!
	m.provisioningMu.Lock()
	prov.Stage = "ready"
	prov.Message = "Desktop is ready!"
	prov.Progress = 100
	prov.Status = "running"
	prov.Ready = true
	m.provisioningMu.Unlock()

	// Retain ready state in provisioning for 5 seconds so in-flight status polls receive ready: true
	time.Sleep(5 * time.Second)
	m.provisioningMu.Lock()
	delete(m.provisioning, sanitizedID)
	m.provisioningMu.Unlock()
}

// GetProvisioning returns the current in-flight provisioning state for a desktop ID if one is active.
func (m *Manager) GetProvisioning(rawID string) (*ProvisioningDesktop, bool) {
	sanitizedID := sanitizeName(rawID)
	m.provisioningMu.RLock()
	defer m.provisioningMu.RUnlock()
	prov, ok := m.provisioning[sanitizedID]
	if !ok {
		return nil, false
	}
	copyProv := *prov
	return &copyProv, true
}

// WaitForGUIReady waits until the desktop container's VNC/websockify port is
// accepting TCP connections, meaning the GUI is initialized and ready for user interaction.
func (m *Manager) WaitForGUIReady(ctx context.Context, rawID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		proxyURL, _, err := m.ProxyTarget(ctx, rawID)
		if err == nil && proxyURL != nil {
			host := proxyURL.Host
			conn, dialErr := net.DialTimeout("tcp", host, 500*time.Millisecond)
			if dialErr == nil {
				conn.Close()
				return true
			}
		}

		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// listInspectConcurrency bounds how many ContainerInspect calls List issues
// at once. List is on the hot path for the dashboard, the JSON API, and
// internal/idlepause's sweep - a control plane managing many desktops must
// not pay for them one inspect at a time, but an unbounded fan-out would
// just move the bottleneck onto the Docker daemon instead.
const listInspectConcurrency = 8

// listCacheTTL is how long a List result is reused. Short enough that a
// change made outside Decurix shows up on the next dashboard poll, long
// enough that a burst of pollers costs one Docker call.
const listCacheTTL = 500 * time.Millisecond

// listCall is one List in flight, shared by everyone who asks meanwhile.
type listCall struct {
	done chan struct{}
	val  []Desktop
	err  error
}

// invalidateList drops the cached List result. Every lifecycle change calls
// it (see the deferred calls in Create/Start/Stop/...), so a caller that acts
// and then lists never reads the state from before its own action.
func (m *Manager) invalidateList() {
	m.listMu.Lock()
	m.listGen++
	m.listVal = nil
	m.listMu.Unlock()
}

// List returns every desktop container plus the ones still being provisioned.
// Cached for listCacheTTL and collapsed across concurrent callers; the
// callers get their own copy of the slice.
func (m *Manager) List(ctx context.Context) ([]Desktop, error) {
	m.listMu.Lock()

	if m.listVal != nil && time.Since(m.listAt) < listCacheTTL {
		out := append([]Desktop(nil), m.listVal...)
		m.listMu.Unlock()
		return out, nil
	}

	if call := m.listFlight; call != nil {
		m.listMu.Unlock()

		select {
		case <-call.done:
			return append([]Desktop(nil), call.val...), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	call := &listCall{done: make(chan struct{})}
	m.listFlight = call
	gen := m.listGen
	m.listMu.Unlock()

	// Detached from this caller's context: the answer is shared, so one
	// impatient client hanging up must not fail everyone waiting on it.
	shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	call.val, call.err = m.listUncached(shared)
	cancel()

	m.listMu.Lock()
	m.listFlight = nil
	if call.err == nil && gen == m.listGen {
		m.listVal = call.val
		m.listAt = time.Now()
	}
	m.listMu.Unlock()
	close(call.done)

	return append([]Desktop(nil), call.val...), call.err
}

// List returns all desktop containers managed by this instance, read from their Docker labels and inspect data.
func (m *Manager) listUncached(
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

	// Each container's own Desktop lands at its own index, written by its
	// own goroutine only - no shared state, so no lock is needed alongside
	// the semaphore.
	result := make([]Desktop, len(containers))

	sem := make(chan struct{}, listInspectConcurrency)
	var wg sync.WaitGroup

	for i, c := range containers {
		wg.Add(1)
		sem <- struct{}{}

		go func(i int, c container.Summary) {
			defer wg.Done()
			defer func() { <-sem }()

			result[i] = m.desktopFromContainer(ctx, c)
		}(i, c)
	}

	wg.Wait()

	// Prepend any active provisioning desktops that haven't registered Docker containers yet
	m.provisioningMu.RLock()
	var pendingProv []Desktop
	for _, p := range m.provisioning {
		if p.Status == "provisioning" {
			found := false
			for _, r := range result {
				if r.ID == p.ID {
					found = true
					break
				}
			}
			if !found {
				pendingProv = append(pendingProv, p.ToDesktop())
			}
		}
	}
	m.provisioningMu.RUnlock()

	if len(pendingProv) > 0 {
		result = append(pendingProv, result...)
	}

	return result, nil
}

// exitCodeRE reads the exit code out of ContainerList's status line,
// "Exited (137) 5 minutes ago".
var exitCodeRE = regexp.MustCompile(`^Exited \((-?\d+)\)`)

// crashReason says why a container in `state` (ContainerList's State, with
// its Status line) stopped on its own, or "" when it did not. Stop ends a
// desktop with SIGTERM (143, or 0 when the init passes it on cleanly) and
// SIGKILLs it once DESKTOP_STOP_TIMEOUT_SECONDS runs out (137), so those are
// a person's or idle-pause's stop, not a crash - except a 137 the kernel's
// OOM killer dealt, which oomKilled (only asked for a 137) tells apart.
func crashReason(state, status string, oomKilled func() bool) string {
	switch state {
	case "dead":
		return "Docker could not stop it cleanly"
	case "restarting":
		return "it keeps restarting"
	case "exited":
	default:
		return ""
	}

	match := exitCodeRE.FindStringSubmatch(status)
	if match == nil {
		return ""
	}

	code, err := strconv.Atoi(match[1])
	if err != nil {
		return ""
	}

	switch code {
	case 0, 143:
		return ""
	case 137:
		if oomKilled != nil && oomKilled() {
			return "it ran out of memory"
		}
		return ""
	}

	return fmt.Sprintf("it exited with code %d", code)
}

// desktopFromContainer builds one Desktop from a ContainerList entry plus its
// metadata labels, falling back to ContainerInspect only for legacy containers
// or on first boot to cache StartedAt.
func (m *Manager) desktopFromContainer(ctx context.Context, c container.Summary) Desktop {
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(
			c.Names[0],
			"/",
		)
	}

	volumeName := ""
	for _, mount := range c.Mounts {
		if mount.Destination == "/home/headless" {
			volumeName = mount.Name
			break
		}
	}

	model := c.Labels["desktop-model"]
	specialization := c.Labels["desktop-specialization"]
	parentID := c.Labels["desktop-parent-id"]

	var cpus int64
	if s := c.Labels["desktop-cpus"]; s != "" {
		cpus, _ = strconv.ParseInt(s, 10, 64)
	}
	var memoryMB int64
	if s := c.Labels["desktop-memory"]; s != "" {
		memoryMB, _ = strconv.ParseInt(s, 10, 64)
	}

	startedAt := ""
	if c.State == "running" {
		m.startedMu.Lock()
		cached, ok := m.startedCache[c.ID]
		m.startedMu.Unlock()
		if ok {
			startedAt = cached
		}
	} else {
		m.startedMu.Lock()
		delete(m.startedCache, c.ID)
		m.startedMu.Unlock()
	}

	// Inspect only if missing critical metadata (legacy container) or if
	// StartedAt is not yet cached for a running container.
	needsInspect := (c.State == "running" && startedAt == "") || (model == "" && c.Labels["desktop-manager"] == "true")
	if needsInspect {
		inspectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		inspect, err := m.client.ContainerInspect(inspectCtx, c.ID)
		cancel()
		if err == nil {
			if inspect.State != nil && inspect.State.Running {
				startedAt = inspect.State.StartedAt
				m.startedMu.Lock()
				m.startedCache[c.ID] = startedAt
				m.startedMu.Unlock()
			}
			if volumeName == "" {
				for _, mount := range inspect.Mounts {
					if mount.Destination == "/home/headless" {
						volumeName = mount.Name
						break
					}
				}
			}
			if model == "" && inspect.Config != nil {
				model = envValue(inspect.Config.Env, "OLLAMA_MODEL")
			}
			if specialization == "" && inspect.Config != nil {
				specialization = envValue(inspect.Config.Env, "DESKTOP_SPECIALIZATION")
				parentID = envValue(inspect.Config.Env, "DESKTOP_PARENT_ID")
			}
			if cpus == 0 && inspect.HostConfig != nil && inspect.HostConfig.NanoCPUs > 0 {
				cpus = inspect.HostConfig.NanoCPUs / 1_000_000_000
			}
			if memoryMB == 0 && inspect.HostConfig != nil && inspect.HostConfig.Memory > 0 {
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
	} else if c.State == "paused" {
		status = "paused"
	}

	crash := crashReason(c.State, c.Status, func() bool {
		inspectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		inspect, err := m.client.ContainerInspect(inspectCtx, c.ID)
		return err == nil && inspect.State != nil && inspect.State.OOMKilled
	})

	id := strings.TrimPrefix(
		name,
		"desktop_",
	)

	m.provisioningMu.RLock()
	if prov, ok := m.provisioning[id]; ok && prov.Status == "provisioning" {
		status = "provisioning"
	}
	m.provisioningMu.RUnlock()

	createdAt := ""
	if c.Created > 0 {
		createdAt = time.Unix(
			c.Created,
			0,
		).Format(time.RFC3339)
	}

	m.resourceMu.Lock()
	if override, ok := m.resourceOverrides[id]; ok {
		cpus = override.CPUs
		memoryMB = override.MemoryMB
	} else if override, ok := m.resourceOverrides[c.ID]; ok {
		cpus = override.CPUs
		memoryMB = override.MemoryMB
	}
	m.resourceMu.Unlock()

	return Desktop{
		ID:             id,
		Name:           id,
		Profile:        profile,
		Harness:        harness,
		Model:          model,
		ContainerID:    c.ID,
		Volume:         volumeName,
		Status:         status,
		URL:            desktopURL(id),
		CreatedAt:      createdAt,
		StartedAt:      startedAt,
		Crash:          crash,
		CPUs:           cpus,
		MemoryMB:       memoryMB,
		Specialization: specialization,
		ParentID:       parentID,
	}
}

// notFound turns Docker's "no such container" into ErrNotFound, so a request
// for a desktop that does not exist is a 404 and not an internal error.
func notFound(id string, err error) error {
	if err != nil && client.IsErrNotFound(err) {
		return fmt.Errorf("find desktop %s: %w", sanitizeName(id), ErrNotFound)
	}
	return err
}

// Start starts or unpauses the Docker container backing the given desktop ID.
func (m *Manager) Start(
	ctx context.Context,
	id string,
) error {
	defer m.invalidateList()

	name := "desktop_" + sanitizeName(id)

	inspect, err := m.client.ContainerInspect(ctx, name)
	if err == nil && inspect.State != nil && inspect.State.Paused {
		return notFound(id, m.client.ContainerUnpause(ctx, name))
	}

	return notFound(id, m.client.ContainerStart(
		ctx,
		name,
		container.StartOptions{},
	))
}

// Pause pauses the Docker container processes backing the given desktop ID using the cgroup freezer.
// It captures and persists the desktop's current screen frame before freezing the container,
// guaranteeing that the last active screen is cached and available while paused.
func (m *Manager) Pause(
	ctx context.Context,
	id string,
) error {
	defer m.invalidateList()

	sanitizedID := sanitizeName(id)
	m.InvalidateProxyTarget(sanitizedID)
	name := "desktop_" + sanitizedID

	// Capture the current screen frame before freezing processes, so the dashboard
	// can show the last active screen while the container is idle/frozen.
	captureCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if data, err := m.Screenshot(captureCtx, sanitizedID, 480); err == nil && len(data) > 0 {
		saveShotToDisk(sanitizedID, data)
	}
	cancel()

	return notFound(id, m.client.ContainerPause(ctx, name))
}

// Unpause unpauses the Docker container processes backing the given desktop ID.
func (m *Manager) Unpause(
	ctx context.Context,
	id string,
) error {
	defer m.invalidateList()

	name := "desktop_" + sanitizeName(id)
	return notFound(id, m.client.ContainerUnpause(ctx, name))
}

// Stop stops the Docker container backing the given desktop ID.
func (m *Manager) Stop(
	ctx context.Context,
	id string,
) error {
	defer m.invalidateList()

	sanitizedID := sanitizeName(id)
	m.InvalidateProxyTarget(sanitizedID)
	name := "desktop_" + sanitizedID

	inspect, inspectErr := m.client.ContainerInspect(ctx, name)
	if inspectErr == nil && inspect.State != nil && inspect.State.Paused {
		_ = m.client.ContainerUnpause(ctx, name)
	}

	timeoutEnv := os.Getenv("DESKTOP_STOP_TIMEOUT_SECONDS")
	timeout := 3
	if timeoutEnv != "" {
		if parsedTimeout, err := strconv.Atoi(timeoutEnv); err == nil && parsedTimeout > 0 {
			timeout = parsedTimeout
		}
	}

	err := m.client.ContainerStop(
		ctx,
		name,
		container.StopOptions{
			Timeout: &timeout,
		},
	)

	m.startedMu.Lock()
	if inspectErr == nil && inspect.ID != "" {
		delete(m.startedCache, inspect.ID)
	}
	delete(m.startedCache, name)
	m.startedMu.Unlock()

	return notFound(id, err)
}

// Restart stops and restarts the Docker container backing the given desktop ID.
func (m *Manager) Restart(
	ctx context.Context,
	id string,
) error {
	defer m.invalidateList()

	sanitizedID := sanitizeName(id)
	m.InvalidateProxyTarget(sanitizedID)
	name := "desktop_" + sanitizedID

	inspect, inspectErr := m.client.ContainerInspect(ctx, name)
	if inspectErr == nil && inspect.State != nil && inspect.State.Paused {
		_ = m.client.ContainerUnpause(ctx, name)
	}

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

	m.startedMu.Lock()
	if inspectErr == nil && inspect.ID != "" {
		delete(m.startedCache, inspect.ID)
	}
	delete(m.startedCache, name)
	m.startedMu.Unlock()

	return notFound(id, m.client.ContainerStart(
		ctx,
		name,
		container.StartOptions{},
	))
}

// Delete removes the Docker container and persistent volume backing the given desktop ID.
func (m *Manager) Delete(
	ctx context.Context,
	id string,
) error {
	defer m.invalidateList()

	sanitizedID := sanitizeName(id)
	m.InvalidateProxyTarget(sanitizedID)
	name := "desktop_" + sanitizedID

	m.provisioningMu.Lock()
	_, isProvisioning := m.provisioning[sanitizedID]
	if isProvisioning {
		delete(m.provisioning, sanitizedID)
	}
	m.provisioningMu.Unlock()

	inspect, err := m.client.ContainerInspect(
		ctx,
		name,
	)
	if err != nil {
		if isProvisioning {
			// Container was not yet created or partially created; clean up volume if any
			volumeName := "decurix_home_" + sanitizedID
			_ = m.client.VolumeRemove(ctx, volumeName, true)
			return nil
		}
		return notFound(id, err)
	}

	if inspect.State != nil && inspect.State.Paused {
		_ = m.client.ContainerUnpause(ctx, name)
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

	m.startedMu.Lock()
	delete(m.startedCache, inspect.ID)
	delete(m.startedCache, name)
	m.startedMu.Unlock()

	m.shotMu.Lock()
	delete(m.shots, sanitizeName(id))
	m.shotMu.Unlock()

	m.viewedMu.Lock()
	delete(m.viewed, sanitizeName(id))
	m.viewedMu.Unlock()

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

// Get returns one desktop, as List would describe it, plus the chat model its
// container was created with (Desktop.Model).
func (m *Manager) Get(ctx context.Context, id string) (Desktop, error) {
	desktopID := sanitizeName(id)

	m.provisioningMu.RLock()
	if prov, ok := m.provisioning[desktopID]; ok && prov.Status == "provisioning" {
		d := prov.ToDesktop()
		m.provisioningMu.RUnlock()
		return d, nil
	}
	m.provisioningMu.RUnlock()

	inspect, err := m.client.ContainerInspect(ctx, "desktop_"+desktopID)
	if err != nil {
		return Desktop{}, fmt.Errorf("find desktop %s: %w", desktopID, ErrNotFound)
	}

	found := Desktop{
		ID:          desktopID,
		Name:        desktopID,
		Profile:     "basic",
		Harness:     HarnessOllama,
		ContainerID: inspect.ID,
		Status:      "stopped",
		URL:         desktopURL(desktopID),
		CreatedAt:   inspect.Created,
	}

	for _, mount := range inspect.Mounts {
		if mount.Destination == "/home/headless" {
			found.Volume = mount.Name
			break
		}
	}

	if inspect.Config != nil {
		if profile := inspect.Config.Labels["desktop-profile"]; profile != "" {
			found.Profile = profile
		}

		if harness := inspect.Config.Labels["desktop-harness"]; harness != "" {
			found.Harness = harness
		}

		found.Model = envValue(inspect.Config.Env, "OLLAMA_MODEL")
		found.Specialization = envValue(inspect.Config.Env, "DESKTOP_SPECIALIZATION")
		found.ParentID = envValue(inspect.Config.Env, "DESKTOP_PARENT_ID")
	}

	if inspect.HostConfig != nil {
		if inspect.HostConfig.NanoCPUs > 0 {
			found.CPUs = inspect.HostConfig.NanoCPUs / 1_000_000_000
		}

		if inspect.HostConfig.Memory > 0 {
			found.MemoryMB = inspect.HostConfig.Memory / (1024 * 1024)
		}
	}

	if inspect.State != nil && inspect.State.Running {
		found.Status = "running"
		found.StartedAt = inspect.State.StartedAt
	}

	m.provisioningMu.RLock()
	if prov, ok := m.provisioning[desktopID]; ok && prov.Status == "provisioning" {
		found.Status = "provisioning"
	}
	m.provisioningMu.RUnlock()

	return found, nil
}

// touchViewed records that someone just looked at a desktop's thumbnail or
// screen right now. Best-effort and fire-and-forget - a missed touch just
// means idlepause has one less signal, never an error a caller has to
// handle.
func (m *Manager) touchViewed(sanitizedID string) {
	m.viewedMu.Lock()
	defer m.viewedMu.Unlock()

	if m.viewed == nil {
		m.viewed = make(map[string]time.Time)
	}

	m.viewed[sanitizedID] = time.Now()
}

// MarkViewed records that a desktop is being used through something other than its thumbnail or proxied screen - an
// app it is showing from the host, say - so idle-pause does not freeze it out from under whoever is using that.
func (m *Manager) MarkViewed(id string) {
	m.touchViewed(sanitizeName(id))
}

// LastViewed returns when a desktop's thumbnail was last fetched or its
// screen last proxied to, or ok=false if this process has never seen either
// for that ID (a desktop nobody has opened since the control plane started).
func (m *Manager) LastViewed(id string) (t time.Time, ok bool) {
	m.viewedMu.Lock()
	defer m.viewedMu.Unlock()

	t, ok = m.viewed[sanitizeName(id)]

	return t, ok
}

// envValue returns one variable from a container's environment list.
func envValue(env []string, name string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}

	return ""
}

// SendRequest posts a JSON request to a running desktop's internal request
// endpoint. taskID is the control plane's record the request belongs to; the
// desktop echoes it back on every step and outcome it reports for that task.
func (m *Manager) SendRequest(
	ctx context.Context,
	id string,
	content string,
	taskID string,
) error {

	desktopID := sanitizeName(id)
	name := "desktop_" + desktopID

	// Verify that the desktop container exists.
	inspect, err := m.client.ContainerInspect(
		ctx,
		name,
	)
	if err != nil {
		return fmt.Errorf(
			"find desktop %s: %w",
			desktopID,
			ErrNotFound,
		)
	}

	// Make sure the desktop is running.
	if inspect.State == nil || !inspect.State.Running {
		return fmt.Errorf(
			"desktop %s: %w",
			desktopID,
			ErrNotRunning,
		)
	}

	if inspect.State.Paused {
		if err := m.client.ContainerUnpause(ctx, name); err != nil {
			return fmt.Errorf("auto-wake desktop %s: %w", desktopID, err)
		}
	}

	payload := map[string]string{
		"content": content,
	}

	if taskID != "" {
		payload["task_id"] = taskID
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	// Communicate directly over the Docker network.
	// vm_server.py listens on port 8081 inside the desktop container.
	url := fmt.Sprintf(
		"http://%s:8081/request",
		name,
	)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		strings.NewReader(string(body)),
	)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set(
		"Content-Type",
		"application/json",
	)

	cli := m.httpClient
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := cli.Do(req)
	if err != nil {
		// Nothing listening on :8081 inside a running container. Reported
		// as ErrAgentUnreachable rather than a bare transport error so the
		// caller can say so, instead of answering a human's chat request
		// with an unexplained HTTP 500.
		return fmt.Errorf(
			"send request to desktop %s: %w: %v",
			desktopID,
			ErrAgentUnreachable,
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
	// A container keeps the image it was created from, so a desktop created
	// before the agent's directories moved under ~/.decurix still runs an
	// agent that reads ~/memory. Write there while it is the only memory
	// directory the desktop has; a newer agent moves it in when it starts.
	memoryDir := "/home/headless/.decurix/memory"
	legacyMemoryDir := "/home/headless/memory"
	delimiter := fmt.Sprintf("DECURIX_MEMORY_%d", time.Now().UnixNano())

	cmd := fmt.Sprintf(`dir=%s; if [ -d %s ] && [ ! -d "$dir" ]; then dir=%s; fi
mkdir -p "$dir" && cat <<'%s' > "$dir/%s"
%s
%s`,
		memoryDir,
		legacyMemoryDir,
		legacyMemoryDir,
		delimiter,
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

	return waitExec(ctx, m.client, execID.ID, fmt.Sprintf("memory write exec for %s", containerName))
}

// waitExec polls ContainerExecInspect until the exec process exits or ctx is cancelled.
func waitExec(ctx context.Context, cli *client.Client, execID, desc string) error {
	for {
		inspect, err := cli.ContainerExecInspect(ctx, execID)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", desc, err)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return fmt.Errorf("%s exited %d", desc, inspect.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// UpdateResources dynamically updates the CPU and memory limits of a desktop container
// without restarting it, using Docker's ContainerUpdate API.
func (m *Manager) UpdateResources(
	ctx context.Context,
	id string,
	cpus int64,
	memoryMB int64,
) error {
	defer m.invalidateList()
	sanitizedID := sanitizeName(id)
	name := "desktop_" + sanitizedID

	swappinessZero := int64(0)
	updateConfig := container.UpdateConfig{
		Resources: container.Resources{
			NanoCPUs:          cpus * 1_000_000_000,
			CPUShares:         cpus * 1024,
			Memory:            memoryMB * 1024 * 1024,
			MemoryReservation: (memoryMB * 1024 * 1024) / 2,
			MemorySwappiness:  &swappinessZero,
		},
	}

	resp, err := m.client.ContainerUpdate(ctx, name, updateConfig)
	if err != nil {
		return fmt.Errorf("update container resources for %s: %w", name, err)
	}
	if len(resp.Warnings) > 0 {
		log.Printf("container %s resource update warnings: %v", name, resp.Warnings)
	}

	m.resourceMu.Lock()
	m.resourceOverrides[sanitizedID] = ResourceAllocation{CPUs: cpus, MemoryMB: memoryMB}
	m.resourceMu.Unlock()

	return nil
}

// Stats returns a single snapshot of container resource usage statistics.
func (m *Manager) Stats(
	ctx context.Context,
	id string,
) (*container.StatsResponse, error) {
	sanitizedID := sanitizeName(id)
	name := "desktop_" + sanitizedID

	reader, err := m.client.ContainerStatsOneShot(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("fetch container stats for %s: %w", name, err)
	}
	defer reader.Body.Close()

	var stats container.StatsResponse
	if err := json.NewDecoder(reader.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("decode container stats for %s: %w", name, err)
	}

	return &stats, nil
}
