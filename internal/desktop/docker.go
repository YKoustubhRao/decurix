package desktop

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/YKoustubhRao/flowera/internal/ollama"
)

const (
	containerPort = "6901/tcp"

	// agentPort is TheBot soul's own HTTP port (SOUL_PORT in
	// start_agent_terminal.sh) - only listening inside desktops created
	// with harness=thebot. Proxied separately from containerPort (VNC)
	// via AgentProxyTarget/agentProxyHandler so the control plane's own
	// UI can offer a real "talk to the agent" tab instead of only VNC.
	agentPort = "4900/tcp"

	desktopImageBasic     = "flowera-desktop:basic"
	desktopImageDeveloper = "flowera-desktop:developer"
	desktopImageAI        = "flowera-desktop:ai"

	// NetworkName is the shared Docker bridge network desktop containers
	// (and the shared Ollama container) are attached to.
	NetworkName = "flowera-network"

	// Agent harnesses: which reasoning engine drives a desktop's tasks.
	HarnessOllama = "ollama"
	HarnessClaude = "claude"
	// HarnessTheBot runs the same OLLAMA_URL/OLLAMA_MODEL every desktop
	// already gets (see buildDesktopEnv) through TheBot's own atomizer/
	// scheduler/skills loop instead of the hand-rolled Ollama loop -
	// needs no new env var of its own for the Ollama case.
	HarnessTheBot = "thebot"
)

// EnsureImages reports whether every desktop image is present locally. It
// deliberately does not pull: flowera-desktop:* are local-only tags that exist
// in no registry, so a pull can only ever fail. Missing images are built from
// the embedded build context by BuildImages.
func (m *Manager) EnsureImages(ctx context.Context) error {
	missing := m.MissingImages(ctx)

	if len(missing) == 0 {
		log.Println("All desktop images present.")
		return nil
	}

	return fmt.Errorf(
		"desktop images not built yet: %s",
		strings.Join(missing, ", "),
	)
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

// buildDesktopEnv builds the container environment variables (VNC, desktop identity, control-plane/Ollama URLs, agent harness) for a desktop by name.
func buildDesktopEnv(name, harness string) []string {
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
		"CONTROL_PLANE_URL=http://host.docker.internal:8080",
		"OLLAMA_URL=" + ollamaURL,
		"OLLAMA_MODEL=" + ollamaModel,
		"AGENT_HARNESS=" + harness,
	}

	return env
}

// buildContainerConfigs builds the Docker container, host, and network configuration for a desktop from its resolved image, resources, profile, harness, and security options.
func buildContainerConfigs(imageName, containerName, profile, harness string, cpus, memoryMB int64, env []string, volumeName string, securityOpt []string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	config := &container.Config{
		Image:    imageName,
		Env:      env,
		Hostname: containerName,
		Labels: map[string]string{
			"desktop-manager": "true",
			"desktop-profile": profile,
			"desktop-harness": harness,
		},
	}

	hostConfig := &container.HostConfig{
		ShmSize:     512 * 1024 * 1024,
		SecurityOpt: securityOpt,
		Resources: container.Resources{
			NanoCPUs: cpus * 1_000_000_000,
			Memory:   memoryMB * 1024 * 1024,
			Ulimits:  desktopUlimits(),
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
