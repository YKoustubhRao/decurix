package desktop

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
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

	// CacheVolumeName is the shared writable volume mounted into desktop containers
	// at /var/cache/decurix to deduplicate Go modules, npm downloads, and cargo registries.
	CacheVolumeName = "decurix-cache"

	// NetworkName is the shared Docker bridge network desktop containers
	// (and the shared Ollama container) are attached to.
	NetworkName = "decurix-network"

	// Agent harnesses: which reasoning engine drives a desktop's tasks.
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
)

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

// ContainerNetworks returns the address ranges of the Docker networks
// desktops live on (decurix-network, and the default bridge), and each one's
// gateway. A request from inside one of those ranges that is not its
// gateway comes from a container - a desktop, or the bundled Ollama - never
// from a person's browser, which reaches the control plane from the host
// (loopback, or through a published port, which Docker hands over from the
// gateway). httpapi uses it to keep what only a person should see away from
// the agents.
func (m *Manager) ContainerNetworks(ctx context.Context) (subnets []netip.Prefix, gateways []netip.Addr) {
	for _, name := range []string{NetworkName, "bridge"} {
		info, err := m.client.NetworkInspect(ctx, name, network.InspectOptions{})
		if err != nil {
			continue
		}

		for _, cfg := range info.IPAM.Config {
			if prefix, err := netip.ParsePrefix(cfg.Subnet); err == nil {
				subnets = append(subnets, prefix.Masked())
			}

			if gw, err := netip.ParseAddr(cfg.Gateway); err == nil {
				gateways = append(gateways, gw)
			}
		}
	}

	return subnets, gateways
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

// buildDesktopEnv builds the container environment variables (VNC, desktop identity, control-plane/Ollama URLs, agent harness) for a desktop by name.
func buildDesktopEnv(name, harness, specialization, parentID string) []string {
	defaultPassword := os.Getenv("DEFAULT_PASSWORD")
	if defaultPassword == "" {
		defaultPassword = "admin"
	}

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
	ollamaModel := ollama.DesktopModel()

	env := []string{
		"VNC_PW=" + defaultPassword,
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
	numThreads := os.Getenv("OLLAMA_NUM_THREAD")
	if numThreads == "" {
		numThreads = strconv.Itoa(ollama.DefaultNumThread())
	}
	env = append(env, "OLLAMA_NUM_THREAD="+numThreads)

	kvCache := os.Getenv("OLLAMA_KV_CACHE_TYPE")
	if kvCache == "" {
		kvCache = "q8_0"
	}
	env = append(env, "OLLAMA_KV_CACHE_TYPE="+kvCache)

	flashAttn := os.Getenv("OLLAMA_FLASH_ATTENTION")
	if flashAttn == "" {
		flashAttn = "1"
	}
	env = append(env, "OLLAMA_FLASH_ATTENTION="+flashAttn)

	if numBatch := os.Getenv("OLLAMA_NUM_BATCH"); numBatch != "" {
		env = append(env, "OLLAMA_NUM_BATCH="+numBatch)
	}

	numParallel := os.Getenv("OLLAMA_NUM_PARALLEL")
	if numParallel == "" {
		numParallel = "1"
	}
	env = append(env, "OLLAMA_NUM_PARALLEL="+numParallel)

	// Every harness asks the hosted model its one-off questions (the replay
	// grounder, judge, detours) when the control plane has one: they are
	// exactly the calls a small local model gets wrong. The key stays in the
	// control plane; this only says the proxy route will answer.
	if strings.TrimSpace(os.Getenv("AZURE_KEY")) != "" && strings.TrimSpace(os.Getenv("AZURE_ENDPOINT")) != "" {
		env = append(env, "DECURIX_LLM=1")
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
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONUNBUFFERED=1",
	)

	return env
}

// buildContainerConfigs builds the Docker container, host, and network configuration for a desktop from its resolved image, resources, profile, harness, and security options.
func buildContainerConfigs(imageName, containerName, profile, harness string, cpus, memoryMB int64, env []string, volumeName string, securityOpt []string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	labels := map[string]string{
		"desktop-manager":        "true",
		"desktop-profile":        profile,
		"desktop-harness":        harness,
		"desktop-cpus":           strconv.FormatInt(cpus, 10),
		"desktop-memory":         strconv.FormatInt(memoryMB, 10),
		"desktop-model":          envValue(env, "OLLAMA_MODEL"),
		"desktop-specialization": envValue(env, "DESKTOP_SPECIALIZATION"),
		"desktop-parent-id":      envValue(env, "DESKTOP_PARENT_ID"),
	}

	config := &container.Config{
		Image:    imageName,
		Env:      env,
		Hostname: containerName,
		Labels:   labels,
	}

	swappinessZero := int64(0)
	shmBytes := int64(512 * 1024 * 1024)
	if shmEnv := os.Getenv("DECURIX_DESKTOP_SHM_MB"); shmEnv != "" {
		if parsed, err := strconv.ParseInt(shmEnv, 10, 64); err == nil && parsed > 0 {
			shmBytes = parsed * 1024 * 1024
		}
	}
	hostConfig := &container.HostConfig{
		ShmSize:     shmBytes,
		SecurityOpt: securityOpt,
		Resources: container.Resources{
			NanoCPUs:          cpus * 1_000_000_000,
			CPUShares:         cpus * 1024,
			Memory:            memoryMB * 1024 * 1024,
			MemoryReservation: (memoryMB * 1024 * 1024) / 2,
			MemorySwappiness:  &swappinessZero,
			Ulimits:           desktopUlimits(),
		},
		Tmpfs: map[string]string{
			"/tmp": "rw,nosuid,size=256m,mode=1777",
			"/run": "rw,nosuid,size=64m,mode=755",
		},
		Binds: []string{
			volumeName + ":/home/headless",
			ToolsVolumeName + ":/opt/tools:ro",
			CacheVolumeName + ":/var/cache/decurix:rw",
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

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {},
		},
	}

	return config, hostConfig, networkingConfig
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
