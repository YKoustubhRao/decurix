package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Manager owns the Docker client and the desktop containers it manages.
type Manager struct {
	// client is the Docker API. It is the interface *client.Client implements, so a test can stand a fake in for the
	// daemon (newManagerWith) and see every call the Manager makes.
	client client.APIClient

	// docker is what the last check of the daemon found (dockerwatch.go).
	docker dockerState

	// callers maps a desktop container's address to its desktop ID (caller.go).
	callers callerCache

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

	// toolsMu/toolsApplied: the tools image ID the shared decurix-tools volume was last filled from, so it is filled
	// again only when the image changed (a tools image built after start, F-T), never on every creation.
	// toolsPopulated skips the check altogether (tests whose daemon has no tools image).
	toolsMu        sync.Mutex
	toolsApplied   string
	toolsPopulated bool
	// toolsWaiting: the last populate found a newer tools image but left the volume as it was, because desktops
	// were running (tools.go); the status banner says so until a later populate applies it.
	toolsWaiting atomic.Bool

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
	// lastCPU is each desktop's previous CPU sample (Stats), under resourceMu.
	lastCPU map[string]container.CPUStats

	// oomMu guards oom, each stopped container's OOM verdict (oomKilled).
	oomMu sync.Mutex
	oom   map[string]bool

	// lifecycleMu guards lifecycle, one lock per desktop (LockDesktop).
	lifecycleMu sync.Mutex
	lifecycle   map[string]*sync.Mutex

	// httpClient pools persistent HTTP connections (Keep-Alive) to desktop
	// agent APIs on port 8081, eliminating TCP handshake overhead on every
	// command dispatch.
	httpClient *http.Client

	// provisioningMu and provisioning track in-flight desktop creation so that
	// requests are accepted immediately (< 15ms) while the container spins up
	// and desktop icons/apps are migrated in the background.
	provisioningMu sync.RWMutex
	provisioning   map[string]*ProvisioningDesktop
	// provisioningRuns is each creation in flight's cancel and its done channel (closed when its goroutine has
	// finished, cleanup included), so a delete can stop a creation and wait for it (Delete).
	provisioningRuns map[*ProvisioningDesktop]provisioningRun

	// missingMu guards missing (the requested host apps each desktop does not have, and why) and retrying (desktops
	// with a background retry of apps the host could not be asked about yet). See missingapps.go.
	missingMu sync.Mutex
	missing   map[string][]MissingApp
	retrying  map[string]bool
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
	if cli == nil {
		return newManagerWith(nil)
	}
	return newManagerWith(cli)
}

// newManagerWith is NewManager over any Docker API: the daemon itself, or a test's fake. A nil api stays a nil
// interface, so the "no Docker" checks (m.client == nil) still hold.
func newManagerWith(api client.APIClient) *Manager {
	return &Manager{
		client:            api,
		startedCache:      make(map[string]string),
		proxyTargets:      make(map[string]proxyTargetCacheEntry),
		resourceOverrides: make(map[string]ResourceAllocation),
		provisioning:      make(map[string]*ProvisioningDesktop),
		missing:           make(map[string][]MissingApp),
		retrying:          make(map[string]bool),
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

// BeginProvisioning registers an in-flight provisioning desktop and launches
// an asynchronous background goroutine to create the volume, container, host app
// migration, container start, and readiness probing. It returns the ProvisioningDesktop
// immediately (< 15ms) so that HTTP APIs and user interfaces never hang.
func (m *Manager) BeginProvisioning(
	name string,
	profile string,
	harness string,
	brain Brain,
	cpus int64,
	memoryMB int64,
	claudeCredentialsJSON string,
	specialization string,
	parentID string,
	selectedApps []string,
	hostPaths []HostCopyPath,
	onStarted func(d *Desktop) error,
	shared ...*SharedFolder,
) (*ProvisioningDesktop, error) {
	var sharedFolder *SharedFolder
	if len(shared) > 0 {
		sharedFolder = shared[0]
	}

	sanitizedID := sanitizeName(name)
	if sanitizedID == "" {
		return nil, fmt.Errorf("invalid desktop name")
	}
	// Host paths are no longer copied into a new desktop (the copy ran as root on a path the worker could shape,
	// B-01); a desktop's one way to the PC's files is its shared folder.
	if len(hostPaths) > 0 {
		return nil, fmt.Errorf("copying folders of this computer into a desktop is not offered; give it a shared folder instead")
	}
	// Without the control plane's secret a desktop would get no agent token and a guessable screen password: its
	// agent would take requests and replies from anyone on the network (review 2026-11-02). Never made that way.
	if len(m.HostGUISecret) == 0 {
		return nil, fmt.Errorf("the control plane could not load its secret (hostgui.key in the data folder), so a new desktop cannot be made safely; check the data folder and restart Decurix")
	}

	// Verify no existing container or provisioning desktop with this name
	m.provisioningMu.RLock()
	if _, exists := m.provisioning[sanitizedID]; exists {
		m.provisioningMu.RUnlock()
		return nil, fmt.Errorf("desktop %s is already being created", name)
	}
	m.provisioningMu.RUnlock()

	// Check if container already exists. Only Docker's own "no such container" lets a creation go on: an inspect
	// that timed out (memory pressure) said nothing, and going on made the create fail on the name and its cleanup
	// remove the existing desktop and its home (B-03).
	if _, err := m.client.ContainerInspect(context.Background(), "desktop_"+sanitizedID); err == nil {
		return nil, alreadyExists(name, sanitizedID)
	} else if !client.IsErrNotFound(err) {
		log.Printf("desktop %s: checking whether it exists: %v", sanitizedID, err)
		return nil, fmt.Errorf("could not check whether desktop %s already exists (Docker did not answer); nothing was changed, try again", sanitizedID)
	}

	prov := &ProvisioningDesktop{
		creation:       newCreationID(),
		ID:             sanitizedID,
		Name:           name,
		Profile:        profile,
		Harness:        harness,
		Brain:          brain.kind(),
		Model:          brain.Model,
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
	if sharedFolder != nil {
		prov.SharedFolder = sharedFolder.Host
	}
	// What is already known about the requested apps goes back with the acceptance; runProvisioning replaces it with
	// what the copy actually found.
	prov.MissingApps = PrecheckHostApps(selectedApps)

	// The caller gets a snapshot, never prov itself: runProvisioning keeps updating prov from its own goroutine (under
	// provisioningMu) for the whole creation, and the caller serialises what it is given without holding that lock -
	// returning the live pointer let the two run at once, a data race on every field of the record.
	// Checked again and claimed under one lock: two requests whose names sanitise alike ("my.box", "my-box") both
	// passed the read-locked check above, and the second one's provisioning removed the first one's container and
	// volume (review 2026-11-02).
	m.provisioningMu.Lock()
	if _, exists := m.provisioning[sanitizedID]; exists {
		m.provisioningMu.Unlock()
		return nil, fmt.Errorf("desktop %s is already being created", name)
	}
	m.provisioning[sanitizedID] = prov
	snapshot := *prov
	m.provisioningMu.Unlock()

	go m.runProvisioning(prov, brain, claudeCredentialsJSON, selectedApps, onStarted, sharedFolder)

	return &snapshot, nil
}

func (m *Manager) runProvisioning(
	prov *ProvisioningDesktop,
	brain Brain,
	claudeCredentialsJSON string,
	selectedApps []string,
	onStarted func(d *Desktop) error,
	shared *SharedFolder,
) {
	defer m.invalidateList()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	done := make(chan struct{})
	m.provisioningMu.Lock()
	if m.provisioningRuns == nil {
		m.provisioningRuns = map[*ProvisioningDesktop]provisioningRun{}
	}
	m.provisioningRuns[prov] = provisioningRun{cancel: cancel, done: done}
	m.provisioningMu.Unlock()
	defer func() {
		m.provisioningMu.Lock()
		delete(m.provisioningRuns, prov)
		m.provisioningMu.Unlock()
		close(done)
	}()

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

	// What this run made, and so all a failure may remove: never anything that was there before it (B-03).
	var createdContainer string
	createdVolume, createdCache := false, false
	cacheName := cacheVolumeFor(containerName)
	fail := func(err error) {
		log.Printf("desktop provisioning failed for %s: %v", sanitizedID, err)
		m.provisioningMu.Lock()
		prov.Status = "failed"
		prov.Error = err.Error()
		prov.Message = fmt.Sprintf("Creation failed: %v", err)
		m.provisioningMu.Unlock()

		if createdContainer != "" {
			_ = m.client.ContainerRemove(context.Background(), createdContainer, container.RemoveOptions{Force: true})
		}
		if createdVolume {
			_ = m.client.VolumeRemove(context.Background(), volumeName, true)
		}
		if createdCache {
			_ = m.client.VolumeRemove(context.Background(), cacheName, true)
		}

		// A creation stopped by a delete is gone at once; a failure is kept 60s so the page can show it.
		if ctx.Err() == context.Canceled {
			m.forgetProvisioning(prov)
			return
		}
		time.AfterFunc(60*time.Second, func() { m.forgetProvisioning(prov) })
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
	made, err := m.homeVolumeFor(ctx, volumeName, sanitizedID, prov.creation)
	createdVolume = made
	if err != nil {
		fail(err)
		return
	}
	made, err = m.cacheVolumeFresh(ctx, cacheName, sanitizedID, prov.creation)
	createdCache = made
	if err != nil {
		fail(err)
		return
	}

	// 3. Container Configuration & Creation
	updateStage("container", "Creating container environment...", 45)
	cpus, memoryMB := m.clampResources(ctx, prov.CPUs, prov.MemoryMB)
	m.provisioningMu.Lock()
	prov.CPUs, prov.MemoryMB = cpus, memoryMB
	m.provisioningMu.Unlock()
	env := buildDesktopEnv(name, prov.Harness, prov.Specialization, prov.ParentID, brain, shared)
	if token := m.AgentToken(name); token != "" {
		env = append(env, AgentTokenEnv+"="+token)
	}
	env = m.withScreenPassword(env, name)
	m.provisioningMu.Lock()
	prov.Model = desktopModel(env)
	m.provisioningMu.Unlock()

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
		shared,
	)

	resp, err := m.client.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, containerName)
	if err != nil {
		fail(fmt.Errorf("create container: %w", err))
		return
	}
	createdContainer = resp.ID
	// The approval key goes in as a root-only file before the first start, never in the environment. A desktop whose
	// image has no gate, or whose key could not be placed, is not started: it would allow nothing.
	if err := m.prepareGate(ctx, resp.ID, m.GateKey(name)); err != nil {
		fail(fmt.Errorf("give the desktop its approval gate: %w", err))
		return
	}

	// 4. Migrate Desktop Icons & Host Apps
	if len(selectedApps) > 0 {
		updateStage("apps", "Copying desktop icons and host apps...", 65)
		if err := m.MigrateHostApps(ctx, sanitizedID, selectedApps); err != nil {
			log.Printf("hostapps: migration for %s: %v", containerName, err)
		}
		m.provisioningMu.Lock()
		prov.MissingApps = m.MissingApps(sanitizedID)
		m.provisioningMu.Unlock()
	}

	// 6. Start Container
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
	if ctx.Err() == context.Canceled {
		// Deleted while it started: the delete waits for this run and then removes what it made.
		m.forgetProvisioning(prov)
		return
	}
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
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
	m.forgetProvisioning(prov)
}

// provisioningRun is one creation's cancel and the channel closed when it has finished.
type provisioningRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// forgetProvisioning removes prov from the creations in flight, only if it is still the one under its ID: a later
// creation of the same name must never lose its entry to an older one's cleanup (review 2026-11-02).
func (m *Manager) forgetProvisioning(prov *ProvisioningDesktop) {
	m.provisioningMu.Lock()
	if m.provisioning[prov.ID] == prov {
		delete(m.provisioning, prov.ID)
	}
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
// it (see the deferred calls in runProvisioning/Start/Stop/...), so a caller that acts
// and then lists never reads the state from before its own action.
func (m *Manager) invalidateList() {
	m.listMu.Lock()
	m.listGen++
	m.listVal = nil
	m.listMu.Unlock()
	// A start or stop can make a new exit: OOM verdicts are asked again (oomKilled).
	m.oomMu.Lock()
	m.oom = nil
	m.oomMu.Unlock()
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
	// The flight is always ended, a panic included (B-32): left set, every later List waited on it until its own
	// context ended.
	func() {
		call.err = errors.New("listing desktops failed")
		defer func() {
			m.listMu.Lock()
			m.listFlight = nil
			if call.err == nil && gen == m.listGen {
				m.listVal = call.val
				m.listAt = time.Now()
			}
			m.listMu.Unlock()
			close(call.done)
		}()
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		call.val, call.err = m.listUncached(shared)
	}()

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

	return m.withProvisioning(result), nil
}

// withProvisioning puts the desktops still being created, which have no
// container (or worker) of their own yet, in front of the ones that exist.
func (m *Manager) withProvisioning(result []Desktop) []Desktop {
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

	return result
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

// oomKilled reports whether container id's last exit was the kernel's OOM killer. The answer is kept per container
// and exit code until the next lifecycle change (invalidateList), so a desktop that stopped with 137 costs one inspect,
// not one on every uncached List (B-33). A failed inspect is not kept.
func (m *Manager) oomKilled(ctx context.Context, id, status string) bool {
	if i := strings.Index(status, ")"); i >= 0 {
		status = status[:i] // "Exited (137": the time since the exit is not part of it
	}
	key := id + "\x00" + status
	m.oomMu.Lock()
	v, ok := m.oom[key]
	m.oomMu.Unlock()
	if ok {
		return v
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	inspect, err := m.client.ContainerInspect(inspectCtx, id)
	if err != nil {
		return false
	}
	v = inspect.State != nil && inspect.State.OOMKilled
	m.oomMu.Lock()
	if m.oom == nil || len(m.oom) > 1024 {
		m.oom = map[string]bool{}
	}
	m.oom[key] = v
	m.oomMu.Unlock()
	return v
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

	volumeName := homeVolume(c.Mounts)

	model := c.Labels["desktop-model"]
	specialization := c.Labels["desktop-specialization"]
	parentID := c.Labels["desktop-parent-id"]
	sharedFolder := c.Labels[sharedLabel]

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
				volumeName = homeVolume(inspect.Mounts)
			}
			if model == "" && inspect.Config != nil {
				model = desktopModel(inspect.Config.Env)
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

	brain := c.Labels["desktop-brain"]
	if brain == "" {
		brain = BrainOllama
	}

	status := "stopped"
	if c.State == "running" {
		status = "running"
	} else if c.State == "paused" {
		status = "paused"
	}

	crash := crashReason(c.State, c.Status, func() bool { return m.oomKilled(ctx, c.ID, c.Status) })

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
		Brain:          brain,
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
		SharedFolder:   sharedFolder,
		ParentID:       parentID,
		MissingApps:    m.MissingApps(id),
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
	if inspect, err := m.client.ContainerInspect(ctx, name); err == nil {
		homeCopyPauses.settled(inspect.ID) // a person resumed it: a copy does not pause it again
	}
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

	timeoutEnv := os.Getenv("DESKTOP_STOP_TIMEOUT_SECONDS")
	timeout := 3
	if timeoutEnv != "" {
		if parsedTimeout, err := strconv.Atoi(timeoutEnv); err == nil && parsedTimeout > 0 {
			timeout = parsedTimeout
		}
	}

	return notFound(id, m.stopContainer(ctx, "desktop_"+sanitizedID, timeout))
}

// stopContainer stops a desktop's container, unpausing it first, with timeout seconds to end, and forgets when it
// started.
func (m *Manager) stopContainer(ctx context.Context, name string, timeout int) error {
	inspect, inspectErr := m.client.ContainerInspect(ctx, name)
	if inspectErr == nil && inspect.State != nil && inspect.State.Paused {
		_ = m.client.ContainerUnpause(ctx, name)
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

	// A tools refresh that waited for running desktops applies once the last of them has stopped (F-T).
	if err == nil {
		m.refreshToolsWhenIdle()
	}
	return err
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

	// Container may already be stopped.
	_ = m.stopContainer(ctx, name, 10)

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

	// A creation in flight is stopped and waited for before anything is removed: removing its entry at once let a
	// new desktop of the same name start while the old run still made (or cleaned up) the same container and volume,
	// and the old one's cleanup wiped the new one (review 2026-11-02). Its name stays claimed until the run is done.
	m.provisioningMu.Lock()
	prov, isProvisioning := m.provisioning[sanitizedID]
	run, running := m.provisioningRuns[prov]
	m.provisioningMu.Unlock()
	if running {
		run.cancel()
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Second):
			return fmt.Errorf("desktop %s is still being created and did not stop; try again", id)
		}
	}
	if isProvisioning {
		m.forgetProvisioning(prov)
	}

	inspect, err := m.client.ContainerInspect(
		ctx,
		name,
	)
	if err != nil {
		if isProvisioning {
			// Stopped before its container was made: the run's own cleanup removed what it had made (a volume it
			// created), and nothing else is this delete's to remove (B-03).
			return nil
		}
		return notFound(id, err)
	}

	if inspect.State != nil && inspect.State.Paused {
		_ = m.client.ContainerUnpause(ctx, name)
	}

	volumeName := homeVolume(inspect.Mounts)
	cacheName := ""
	for _, mp := range inspect.Mounts {
		// This desktop's own package cache; never the one older desktops shared (CacheVolumeName).
		if mp.Destination == "/var/cache/decurix" && mp.Name == cacheVolumeFor(name) {
			cacheName = mp.Name
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

	// A desktop made again under this name starts at the size it is given, not the old one's (B-25).
	m.resourceMu.Lock()
	delete(m.resourceOverrides, sanitizedID)
	delete(m.resourceOverrides, inspect.ID)
	delete(m.lastCPU, sanitizedID)
	m.resourceMu.Unlock()

	// A home-copy helper still holding the home volume would keep it from being removed.
	m.removeHomeHelpersOf(ctx, name)

	// Delete persistent storage too.
	if cacheName != "" {
		if err := m.client.VolumeRemove(ctx, cacheName, true); err != nil {
			log.Printf("warning: removing volume %s: %v", cacheName, err)
		}
	}
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
		Brain:       BrainOllama,
		ContainerID: inspect.ID,
		Status:      "stopped",
		URL:         desktopURL(desktopID),
		CreatedAt:   inspect.Created,
	}

	found.Volume = homeVolume(inspect.Mounts)

	if inspect.Config != nil {
		if profile := inspect.Config.Labels["desktop-profile"]; profile != "" {
			found.Profile = profile
		}

		if harness := inspect.Config.Labels["desktop-harness"]; harness != "" {
			found.Harness = harness
		}

		if brain := inspect.Config.Labels["desktop-brain"]; brain != "" {
			found.Brain = brain
		}

		found.Model = desktopModel(inspect.Config.Env)
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

// homeVolume is the name of the volume mounted as a desktop's /home/headless, or "".
func homeVolume(mounts []container.MountPoint) string {
	for _, mount := range mounts {
		if mount.Destination == "/home/headless" {
			return mount.Name
		}
	}
	return ""
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

// RequestBody is one request to a desktop's agent (/request, contract C6). TaskID is the control plane's record
// the request belongs to; the desktop echoes it back on every step and outcome it reports for that task. Secret says
// the records keep it withheld (the person's mark, or what the control plane withheld, C14): the agent is told
// ("secret": true), so it too keeps it out of its logs and records (F-2). JudgementID is the judgement a person's
// reply is, for what the desktop keeps from it to cite (R8). DateOrder is the organisation's date order, "day" or
// "month" ("" unknown).
type RequestBody struct {
	Content     string
	TaskID      string
	Secret      bool
	JudgementID string
	DateOrder   string
}

// SendRequest posts a JSON request to a running desktop's internal request endpoint (RequestBody says what it
// carries; fields left empty are not sent).
func (m *Manager) SendRequest(ctx context.Context, id string, rb RequestBody) error {
	content, taskID, secret := rb.Content, rb.TaskID, rb.Secret

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

	payload := map[string]any{
		"content": content,
	}

	if taskID != "" {
		payload["task_id"] = taskID
	}

	if secret {
		payload["secret"] = true
	}

	if rb.JudgementID != "" {
		payload["judgement_id"] = rb.JudgementID
	}

	if rb.DateOrder == "day" || rb.DateOrder == "month" {
		payload["date_order"] = rb.DateOrder
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	// Communicate directly over the Docker network.
	// vm_server.py listens on port 8081 inside the desktop container. By
	// address, not container name: the name resolves only for a control
	// plane that is itself on decurix-network (compose); the released
	// binary on the host has no Docker DNS, and every request it sent failed
	// as "no such host". The same address resolveProxyTarget uses for the
	// screen and the recordings.
	host := name
	if inspect.NetworkSettings != nil {
		if n, ok := inspect.NetworkSettings.Networks[NetworkName]; ok && n != nil && n.IPAddress != "" {
			host = n.IPAddress
		}
	}
	url := fmt.Sprintf(
		"http://%s:8081/request",
		host,
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
	m.AuthorizeAgentRequest(req, desktopID)

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

// PersistMemoryFile writes pre-formatted memory content into a desktop's memory directory, as the worker: the
// content goes in as a tar on the exec's stdin, never spliced into a shell command (B-31), so no text in it can end
// the file early or run as a command. It works on a stopped desktop too (homefs.go).
func (m *Manager) PersistMemoryFile(
	ctx context.Context,
	desktopID string,
	filename string,
	fileContent string,
) error {
	name := path.Base(filename)
	if name != filename || name == "." || name == ".." || name == "/" || name == "" {
		return fmt.Errorf("memory file %q is not a file name", filename)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte(strings.TrimSpace(fileContent) + "\n")
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now()}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}

	target, done, err := m.homeAccess(ctx, desktopID)
	if err != nil {
		return err
	}
	defer done()
	// A container keeps the image it was created from, so a desktop created before the agent's directories moved
	// under ~/.decurix still runs an agent that reads ~/memory. Write there while it is the only memory directory the
	// desktop has; a newer agent moves it in when it starts. The script is constant; only the archive varies.
	const script = `dir=/home/headless/.decurix/memory
if [ -d /home/headless/memory ] && [ ! -d "$dir" ]; then dir=/home/headless/memory; fi
mkdir -p "$dir" && exec tar -x --no-overwrite-dir -C "$dir" -f -`
	code, err := m.execWorker(ctx, target, &buf, "/bin/sh", "-c", script)
	if err != nil {
		return fmt.Errorf("write memory into desktop %s: %w", sanitizeName(desktopID), err)
	}
	if code != 0 {
		return fmt.Errorf("write memory into desktop %s: exit %d", sanitizeName(desktopID), code)
	}
	return nil
}

// execBash runs cmd with bash (no profile, empty environment: workerCmd) as the worker in a container and polls until it exits or ctx is cancelled; a non-zero exit is an
// error. what names the exec in errors.
func (m *Manager) execBash(ctx context.Context, containerName, cmd, what string) error {
	execID, err := m.client.ContainerExecCreate(ctx, containerName, container.ExecOptions{
		User:         "1001:1001", // the desktop starts as root (agent/decurix-entrypoint.sh); this is the worker's own work
		Cmd:          workerCmd("/bin/bash", "--noprofile", "--norc", "-c", cmd),
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("create %s for %s: %w", what, containerName, err)
	}

	if err := m.client.ContainerExecStart(ctx, execID.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("start %s for %s: %w", what, containerName, err)
	}

	desc := fmt.Sprintf("%s for %s", what, containerName)
	for {
		inspect, err := m.client.ContainerExecInspect(ctx, execID.ID)
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

	cpus, memoryMB = m.clampResources(ctx, cpus, memoryMB)
	resp, err := m.client.ContainerUpdate(ctx, name, container.UpdateConfig{Resources: desktopResources(cpus, memoryMB)})
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
	m.withPreviousCPU(sanitizedID, &stats)

	return &stats, nil
}

// withPreviousCPU gives a one-shot sample the CPU reading to measure against. Docker's one-shot stats leave
// PreCPUStats empty, so the "delta" was everything used since boot and a pegged desktop read 0.35% (B-25): the
// previous sample of the same desktop is used instead, and the first sample of a desktop measures nothing (0%).
func (m *Manager) withPreviousCPU(id string, stats *container.StatsResponse) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	if m.lastCPU == nil {
		m.lastCPU = map[string]container.CPUStats{}
	}
	if stats.PreCPUStats.SystemUsage == 0 {
		if prev, ok := m.lastCPU[id]; ok && prev.SystemUsage <= stats.CPUStats.SystemUsage {
			stats.PreCPUStats = prev
		} else {
			stats.PreCPUStats = stats.CPUStats
		}
	}
	m.lastCPU[id] = stats.CPUStats
}

// clampResources keeps a desktop's CPUs and memory within what the machine has (B-25): asking Docker for more CPUs
// than exist fails, and more memory than exists only promises what cannot be given. Without an answer from Docker
// the request stands as asked.
func (m *Manager) clampResources(ctx context.Context, cpus, memoryMB int64) (int64, int64) {
	info, err := m.client.Info(ctx)
	if err != nil {
		return cpus, memoryMB
	}
	if n := int64(info.NCPU); n > 0 && cpus > n {
		cpus = n
	}
	if total := info.MemTotal / (1024 * 1024); total > 0 && memoryMB > total {
		memoryMB = total
	}
	return cpus, memoryMB
}
