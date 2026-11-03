package desktop

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"

	"github.com/YKoustubhRao/decurix/internal/ollama"
)

const (
	containerPort = "6901/tcp"

	// agentPort is TheBot soul's own HTTP port (SOUL_PORT in
	// start_agent_terminal.sh) - only listening inside desktops created
	// with harness=thebot. Proxied separately from containerPort (VNC)
	// via AgentProxyTarget/agentProxyHandler so the control plane's own
	// UI can offer a real "talk to the agent" tab instead of only VNC.
	agentPort = "4900/tcp"

	desktopImageBasic     = "decurix-desktop:basic"
	desktopImageDeveloper = "decurix-desktop:developer"
	desktopImageAI        = "decurix-desktop:ai"

	// ToolsImage is the lightweight container that builds and populates the shared tools volume.
	ToolsImage = "decurix-tools:latest"

	// ToolsVolumeName is the single shared Docker volume mounted read-only (:ro)
	// into desktop containers at /opt/tools.
	ToolsVolumeName = "decurix-tools"

	// CacheVolumeName was the one writable cache volume every desktop shared at /var/cache/decurix. A desktop
	// created before 2026-11-03 still mounts it (recreate it to stop); a new one gets a cache of its own
	// (cacheVolumeFor) and the read-only model cache (HFVolumeName): a cache one worker writes and another trusts on
	// read is a way from one worker into the other (B-02). Delete never removes it.
	CacheVolumeName = "decurix-cache"

	// NetworkName is the shared Docker bridge network desktop containers
	// (and the shared Ollama container) are attached to.
	NetworkName = "decurix-network"

	// Agent harnesses: which reasoning engine drives a desktop's tasks. New
	// desktops always run TheBot (with a Brain of their own); the others
	// remain only so desktops created before that keep working.
	HarnessOllama = "ollama"
	HarnessClaude = "claude"
	// HarnessTheBot runs the same OLLAMA_URL/OLLAMA_MODEL every desktop
	// already gets (see buildDesktopEnv) through TheBot's own atomizer/
	// scheduler/skills loop instead of the hand-rolled Ollama loop -
	// needs no new env var of its own for the Ollama case.
	HarnessTheBot = "thebot"
	// HarnessAzure is a tool-calling loop on the hosted model the control
	// plane forwards to (internal/llm, /api/llm/chat): the engine for back-
	// office work that has to read documents and screens a 3B local model
	// cannot. The desktop never holds the model's key.
	HarnessAzure = "azure"

	// Brains: which model TheBot reasons with (Desktop.Brain). The hosted
	// ones name an internal/llm provider; the desktop is given the proxy's
	// address, never a key.
	BrainOllama = "ollama"
	BrainClaude = "claude"
	BrainAzure  = "azure"
	// BrainOpenAI is any OpenAI-compatible server the control plane was given (OpenAI itself, OpenRouter, a Brainiac
	// box on the network): the same proxy route, the same env for the desktop.
	BrainOpenAI = "openai"
)

// Brain is a desktop's model choice as the control plane resolved it at
// creation: Kind is one of the Brain* constants ("" is BrainOllama), Model
// the hosted model's name (shown on the card; ignored for BrainOllama).
type Brain struct {
	Kind  string
	Model string
	// Vision is whether a hosted model reads images (tested when its key was
	// saved). For the Ollama brain it is not said: TheBot asks Ollama.
	Vision bool
}

// kind is b.Kind with "" read as BrainOllama.
func (b Brain) kind() string {
	if b.Kind == "" {
		return BrainOllama
	}
	return b.Kind
}

// Hosted reports whether the brain is a hosted model rather than Ollama.
func (b Brain) Hosted() bool {
	return b.kind() != BrainOllama
}

// desktopModel is the model a desktop reasons with, as its env says: the
// hosted brain's when it has one, else the Ollama model.
func desktopModel(env []string) string {
	if m := envValue(env, "DECURIX_BRAIN_MODEL"); m != "" {
		return m
	}
	return envValue(env, "OLLAMA_MODEL")
}

// EnsureImages reports whether every desktop image is present locally. It
// deliberately does not pull: decurix-desktop:* are local-only tags that exist
// in no registry, so a pull can only ever fail. Missing images are built from
// the embedded build context by BuildImages.
func (m *Manager) EnsureImages(ctx context.Context) error {
	missing := m.MissingImages(ctx)

	if len(missing) == 0 {
		log.Println("All desktop images present.")
		if err := m.EnsureToolsVolume(ctx); err != nil {
			log.Printf("warning: ensure tools volume: %v", err)
		}
		return nil
	}

	return fmt.Errorf(
		"desktop images not built yet: %s",
		strings.Join(missing, ", "),
	)
}

// GatewayIPs returns the IPv4 gateway addresses of the networks a desktop can reach the host through (decurix-network
// and Docker's default bridge, which is what host.docker.internal resolves to on Linux) - the addresses a control plane
// running on the host must listen on, besides loopback, for its desktops to call back without opening a port to the LAN.
func (m *Manager) GatewayIPs(ctx context.Context) []string {

	var ips []string
	seen := map[string]bool{}
	// Never the practice network: a practice desktop reaches only its broker, so the control plane listens nowhere on
	// it (review 2026-11-02 - an edit put it here by mistake and bound the no-login listener there).
	for _, name := range []string{NetworkName, "bridge"} {
		info, err := m.client.NetworkInspect(ctx, name, network.InspectOptions{})
		if err != nil {
			continue
		}
		for _, cfg := range info.IPAM.Config {
			ip := net.ParseIP(cfg.Gateway)
			if ip == nil || ip.To4() == nil || seen[cfg.Gateway] {
				continue
			}
			seen[cfg.Gateway] = true
			ips = append(ips, cfg.Gateway)
		}
	}
	return ips
}

// EnsureNetwork creates the shared Docker network used by desktops if it does not already exist.
func (m *Manager) EnsureNetwork(ctx context.Context) error {

	_, err := m.client.NetworkInspect(
		ctx,
		NetworkName,
		network.InspectOptions{},
	)

	if err == nil {
		return nil
	}

	log.Printf("Creating Docker network %s...", NetworkName)

	_, err = m.client.NetworkCreate(
		ctx,
		NetworkName,
		network.CreateOptions{
			Driver: "bridge",
			Labels: map[string]string{
				"decurix": "true",
			},
		},
	)

	if err != nil {
		return fmt.Errorf(
			"create Decurix network: %w",
			err,
		)
	}

	return nil
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

// controlPlaneURL is where a desktop's agent and launchers call back to. A
// control plane running in Docker (docker-compose.yaml) sets
// DECURIX_CONTROL_PLANE_URL to its own name on decurix-network, so callbacks
// travel container to container and the host's published :8080 can stay
// bound to loopback. Anything else - the released binary on the host - is
// reached through host.docker.internal, as before.
func controlPlaneURL() string {
	if u := strings.TrimSpace(os.Getenv("DECURIX_CONTROL_PLANE_URL")); u != "" {
		return u
	}
	return "http://host.docker.internal:8080"
}

// buildDesktopEnv builds the container environment variables (VNC, desktop identity, control-plane/Ollama URLs, agent harness, brain) for a desktop by name.
func buildDesktopEnv(name, harness, specialization, parentID string, brain Brain, shared ...*SharedFolder) []string {

	// OLLAMA_URL/OLLAMA_MODEL follow the same pattern as DEFAULT_PASSWORD:
	// the control plane's own environment wins, with a default that has to
	// work on a machine nobody configured.
	//
	// The default used to be a hardcoded Cloudflare quick-tunnel URL
	// pointing at a developer's own Ollama. Quick-tunnel hostnames are
	// regenerated on every cloudflared restart, so that one stopped
	// resolving and took every unconfigured desktop's reasoning with it -
	// and, because the agent blocked on wait_for_ollama() before binding
	// :8081, presented as an unexplained HTTP 500 on every chat request
	// rather than as "the model is unreachable" (see app.py).
	//
	// host.docker.internal is the host running the control plane, already
	// wired up as an ExtraHosts entry in buildContainerConfigs and already
	// how a desktop reaches CONTROL_PLANE_URL. It points at whatever real
	// Ollama the operator runs there, needs no tunnel, and cannot go stale.
	// It does require that Ollama to listen on more than loopback
	// (OLLAMA_HOST=0.0.0.0); when it doesn't, the failure now arrives as a
	// reply in the chat naming this URL.
	//
	// Both come from package ollama, which preloads the model on this same
	// server at startup - the two must never disagree about which one it is.
	ollamaURL := ollama.DesktopURL()
	if os.Getenv("DECURIX_WINDOWS_OLLAMA_URL") != "" {
		ollamaURL = strings.TrimRight(controlPlaneURL(), "/") + "/api/native-ollama"
	}
	ollamaModel := ollama.DesktopModel()

	env := []string{
		"VNC_RESOLUTION=1280x720",
		// sanitizeName, not an ad-hoc lowercase/space swap: this is the ID
		// the agent reports itself as on every callback, and Desktop.ID is
		// derived from the container name, which sanitizeName produces. The
		// two spellings diverged for any name containing a character only
		// sanitizeName rewrites (a dot, say: "my.box" stayed "my.box" here
		// while the desktop was listed as "my-box"), so that desktop's query
		// notes and chat entries matched no desktop the dashboard knew.
		"DESKTOP_ID=" + sanitizeName(name),
		"DESKTOP_NAME=" + name,
		"CONTROL_PLANE_URL=" + controlPlaneURL(),
		"OLLAMA_URL=" + ollamaURL,
		"OLLAMA_MODEL=" + ollamaModel,
		"AGENT_HARNESS=" + harness,
	}

	// Forward Ollama performance & thread tuning into desktops so agents don't
	// starve the host CPU or thrash caches.
	env = append(env,
		"OLLAMA_NUM_THREAD="+envOr("OLLAMA_NUM_THREAD", strconv.Itoa(ollama.DefaultNumThread())),
		"OLLAMA_KV_CACHE_TYPE="+envOr("OLLAMA_KV_CACHE_TYPE", "q8_0"),
		"OLLAMA_FLASH_ATTENTION="+envOr("OLLAMA_FLASH_ATTENTION", "1"),
	)
	if numBatch := os.Getenv("OLLAMA_NUM_BATCH"); numBatch != "" {
		env = append(env, "OLLAMA_NUM_BATCH="+numBatch)
	}
	env = append(env, "OLLAMA_NUM_PARALLEL="+envOr("OLLAMA_NUM_PARALLEL", "1"))

	env = append(env, "DECURIX_BRAIN="+brain.kind())

	// Every desktop the control plane makes must have an approval gate: its agent and TheBot allow nothing gated
	// rather than fall back to a store the worker can write (agenttoken.go's GateRequiredEnv).
	env = append(env, GateRequiredEnv+"=1")

	// The one folder of the person's computer this desktop may use, when they chose one (sharedfolder.go).
	if len(shared) > 0 && shared[0] != nil {
		env = append(env, shared[0].Env()...)
	}

	// A hosted brain: TheBot's OpenAI-compatible provider pointed at the
	// control plane's proxy (/api/llm/<provider>/v1/...), which adds the key
	// and, for Claude, translates. The "key" here is a placeholder soul's
	// config insists on; the real one never enters a desktop. Embeddings
	// stay on the shared Ollama (EMBED_PROVIDER): Claude has no embeddings
	// API and an Azure resource need not have an embedding deployment.
	//
	// DECURIX_LLM names the same provider for the agent's own one-off
	// questions (harness.ask_model: the replay grounder, judge, detours) -
	// exactly the calls a small local model gets wrong. A desktop on the
	// local brain asks the local model, so it never spends on a hosted one
	// nobody chose for it.
	if brain.Hosted() {
		env = append(env,
			"LLM_PROVIDER=openai",
			"OPENAI_BASE_URL="+strings.TrimRight(controlPlaneURL(), "/")+"/api/llm/"+brain.kind(),
			"OPENAI_API_KEY=held-by-the-control-plane",
			"OPENAI_MODEL=default",
			"EMBED_PROVIDER=ollama",
			"DECURIX_LLM="+brain.kind(),
			"DECURIX_BRAIN_MODEL="+brain.Model,
			"DECURIX_BRAIN_VISION="+map[bool]string{true: "1", false: "0"}[brain.Vision],
			// A hosted model reads long prompts in seconds; the CPU-sized
			// defaults in start_agent_terminal.sh would starve its research.
			"THEBOT_WEB_PAGES=3",
			"THEBOT_WEB_PAGE_CHARS=6000",
		)
	}

	if specialization != "" {
		env = append(env, "DESKTOP_SPECIALIZATION="+specialization)
	}

	if parentID != "" {
		env = append(env, "DESKTOP_PARENT_ID="+parentID)
	}

	env = append(env,
		"PATH=/opt/tools/bin:/opt/tools/go/bin:/opt/tools/node/bin:/opt/tools/rust/bin:/home/headless/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"GOROOT=/opt/tools/go",
		"RUSTUP_HOME=/opt/tools/rust",
		"CARGO_HOME=/home/headless/.cargo",
		"NPM_CONFIG_CACHE=/var/cache/decurix/npm",
		"GOPATH=/home/headless/go",
		"GOMODCACHE=/var/cache/decurix/go",
		"PIP_CACHE_DIR=/var/cache/decurix/pip",
		"HF_HOME=/var/cache/decurix/huggingface",
		// The models the control plane put in the shared, read-only model cache (HFVolumeName).
		"DECURIX_SHARED_MODELS="+HFMountPoint,
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONUNBUFFERED=1",
	)

	return env
}

// envOr is the control plane's own value of the environment variable name, or def when it is unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// buildContainerConfigs builds the Docker container, host, and network configuration for a desktop from its resolved image, resources, profile, harness, and security options.
func buildContainerConfigs(imageName, containerName, profile, harness string, cpus, memoryMB int64, env []string, volumeName string, securityOpt []string, shared ...*SharedFolder) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	labels := map[string]string{
		"desktop-manager":        "true",
		"desktop-profile":        profile,
		"desktop-harness":        harness,
		"desktop-cpus":           strconv.FormatInt(cpus, 10),
		"desktop-memory":         strconv.FormatInt(memoryMB, 10),
		"desktop-brain":          envValue(env, "DECURIX_BRAIN"),
		"desktop-model":          desktopModel(env),
		"desktop-specialization": envValue(env, "DESKTOP_SPECIALIZATION"),
		"desktop-parent-id":      envValue(env, "DESKTOP_PARENT_ID"),
		// The control plane's own record that this desktop runs an approval gate (prepareGate refuses an image
		// without one): what HasGate reads, never a file inside the desktop.
		GateLabel: "1",
	}
	if len(shared) > 0 && shared[0] != nil {
		labels[sharedLabel] = shared[0].Host
	}

	config := &container.Config{
		Image:    imageName,
		Env:      env,
		Hostname: containerName,
		Labels:   labels,
	}

	resources := desktopResources(cpus, memoryMB)
	resources.Ulimits = desktopUlimits()
	shmBytes := int64(512 * 1024 * 1024)
	if shmEnv := os.Getenv("DECURIX_DESKTOP_SHM_MB"); shmEnv != "" {
		if parsed, err := strconv.ParseInt(shmEnv, 10, 64); err == nil && parsed > 0 {
			shmBytes = parsed * 1024 * 1024
		}
	}
	// No process in a desktop gains privileges by exec - not the worker's (the entrypoint's setpriv already sets it),
	// and not one the control plane starts with docker exec, which the entrypoint never sees (B-21).
	securityOpt = append(append([]string{}, securityOpt...), "no-new-privileges:true")
	hostConfig := &container.HostConfig{
		ShmSize:     shmBytes,
		SecurityOpt: securityOpt,
		Resources:   resources,
		// No raw sockets: with them a desktop could forge ARP and take another's address, which the control plane's
		// identity-by-address checks rely on (docs/gate-boundary.md).
		CapDrop: []string{"NET_RAW"},
		Tmpfs: map[string]string{
			"/tmp": "rw,nosuid,size=256m,mode=1777",
			"/run": "rw,nosuid,size=64m,mode=755",
		},
		// Every writable volume is this desktop's alone (B-02): its home and its package caches. What desktops share
		// is read-only: the tools, and the models the control plane put in the model cache.
		Binds: []string{
			volumeName + ":/home/headless",
			ToolsVolumeName + ":/opt/tools:ro",
			cacheVolumeFor(containerName) + ":/var/cache/decurix:rw",
			HFVolumeName + ":" + HFMountPoint + ":ro",
		},
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "10m",
				"max-file": "3",
			},
		},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
		ExtraHosts: []string{
			"host.docker.internal:host-gateway",
		},
	}

	// The shared folder is a mount of its own (not in Binds): the daemon refuses a source that does not exist, where a
	// bind would make it on the host.
	if len(shared) > 0 && shared[0] != nil {
		hostConfig.Mounts = append(hostConfig.Mounts, shared[0].Mount())
	}

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {},
		},
	}

	return config, hostConfig, networkingConfig
}

// desktopIdleMB is the memory a desktop is guaranteed whatever size was chosen: what an idle one uses (measured
// 2026-10-30: ~290 MB with no browser, ~510 MB with one open). The chosen size is a ceiling taken on demand; it was
// half reserved, so an idle 8 GB desktop held 4 GB back from Windows and every other desktop under pressure.
const desktopIdleMB = 512

// desktopReservation is the memory a desktop of memoryMB keeps under pressure: desktopIdleMB, or all of it when smaller.
func desktopReservation(memoryMB int64) int64 {
	return min(memoryMB, desktopIdleMB) * 1024 * 1024
}

// desktopResources is the CPU and memory a desktop is given: cpus whole CPUs and up to memoryMB of memory, only the
// idle floor of it reserved, never swapped.
func desktopResources(cpus, memoryMB int64) container.Resources {
	swappinessZero := int64(0)
	return container.Resources{
		NanoCPUs:          cpus * 1_000_000_000,
		CPUShares:         cpus * 1024,
		Memory:            memoryMB * 1024 * 1024,
		MemoryReservation: desktopReservation(memoryMB),
		MemorySwappiness:  &swappinessZero,
	}
}

// ReleaseReservations lowers the memory reservation of desktops created when half the chosen size was reserved to
// the idle floor, live (Docker's update, no restart), and returns the IDs it changed. A one-off startup pass.
func (m *Manager) ReleaseReservations(ctx context.Context) []string {
	args := filters.NewArgs()
	args.Add("label", "desktop-manager=true")
	containers, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil
	}
	var changed []string
	for _, c := range containers {
		inspect, err := m.client.ContainerInspect(ctx, c.ID)
		if err != nil || inspect.HostConfig == nil || inspect.HostConfig.Memory <= 0 {
			continue
		}
		want := desktopReservation(inspect.HostConfig.Memory / (1024 * 1024))
		if inspect.HostConfig.MemoryReservation <= want {
			continue
		}
		// Only the reservation is sent: Docker leaves a zero field of an update as it is.
		if _, err := m.client.ContainerUpdate(ctx, c.ID, container.UpdateConfig{Resources: container.Resources{MemoryReservation: want}}); err != nil {
			log.Printf("could not release %s's memory reservation: %v", inspect.Name, err)
			continue
		}
		changed = append(changed, strings.TrimPrefix(strings.TrimPrefix(inspect.Name, "/"), "desktop_"))
	}
	return changed
}

// SanitizeID normalizes a caller-supplied desktop identifier the same way
// container and volume names are normalized, so a key derived from it outside
// this package (the chat store's, for one) always agrees with Desktop.ID.
func SanitizeID(rawID string) string {
	return sanitizeName(rawID)
}

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

// desktopURL returns the root-relative URL for a desktop's screen, served by
// the control plane's /desktop/<id>/ reverse proxy (which maps the bare path
// to the underlying noVNC vnc.html internally - the filename never appears in
// the URL a user sees). It is relative on purpose: the browser resolves it
// against whatever origin the dashboard was opened on (localhost, a LAN IP,
// the Cloudflare tunnel), so a phone is never sent to its own localhost and an
// https page never embeds an http:// iframe.
func desktopURL(id string) string {
	return "/desktop/" + id + "/"
}

// StaleControlPlane returns the IDs of desktops whose baked-in CONTROL_PLANE_URL is not the one new desktops get now.
// A container's environment is fixed when it is created, so such a desktop keeps calling the old address - which no
// longer works once the control plane stops listening on the LAN-facing one. It is a one-off startup report, not a
// per-request check.
func (m *Manager) StaleControlPlane(ctx context.Context) []string {

	args := filters.NewArgs()
	args.Add("label", "desktop-manager=true")
	containers, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil
	}
	want := controlPlaneURL()
	var stale []string
	for _, c := range containers {
		inspect, err := m.client.ContainerInspect(ctx, c.ID)
		if err != nil || inspect.Config == nil {
			continue
		}
		if have := envValue(inspect.Config.Env, "CONTROL_PLANE_URL"); have != "" && have != want {
			stale = append(stale, strings.TrimPrefix(strings.TrimPrefix(inspect.Name, "/"), "desktop_"))
		}
	}
	return stale
}
