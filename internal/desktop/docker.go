package desktop

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
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

	// Real bug found testing this live: OLLAMA_URL was hardcoded to the
	// bundled flowera-ollama container - a stub with a single 0.6b model,
	// never a real one - unlike DEFAULT_PASSWORD/ANTHROPIC_API_KEY just
	// below, which already read from the control plane's own environment.
	// Every desktop it created was structurally unable to reach a real
	// Ollama server regardless of what the harness itself wanted.
	// Same fallback pattern now: override via the control plane's own
	// OLLAMA_URL/OLLAMA_MODEL env when set. The default below is
	// TEMPORARY: it points at the user's own Cloudflare-tunneled real
	// Ollama server (see start_agent_terminal.sh's matching fallback) so
	// every desktop gets a real model out of the box without extra
	// per-deployment config, until Flowera has its own real Ollama
	// server. Swap back to the bundled flowera-ollama stub, or drop the
	// default entirely, once that's in place.
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "https://art-sized-symposium-instructor.trycloudflare.com"
	}
	// Swapped from qwen2.5-coder:14b to devstral-small-2:24b per the user -
	// "brain swap" test, gemma4:26b (also available on the tunnel) wasn't
	// good enough. Still just a default: OLLAMA_MODEL above always wins.
	ollamaModel := os.Getenv("OLLAMA_MODEL")
	if ollamaModel == "" {
		ollamaModel = "devstral-small-2:24b"
	}

	env := []string{
		"VNC_PW=" + defaultPassword,
		"VNC_RESOLUTION=1280x720",
		"DESKTOP_ID=" + strings.ToLower(
			strings.ReplaceAll(name, " ", "-"),
		),
		"DESKTOP_NAME=" + name,
		"CONTROL_PLANE_URL=http://host.docker.internal:8080",
		"OLLAMA_URL=" + ollamaURL,
		"OLLAMA_MODEL=" + ollamaModel,
		"AGENT_HARNESS=" + harness,
	}

	if apiKey := os.Getenv("ANTHROPIC_API_KEY"); apiKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+apiKey)
	}

	return env
}

// buildContainerConfigs builds the Docker container, host, and network configuration for a desktop from its resolved image, resources, profile, and harness.
func buildContainerConfigs(imageName, containerName, profile, harness string, cpus, memoryMB int64, env []string, volumeName string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
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
			NetworkName: {},
		},
	}

	return config, hostConfig, networkingConfig
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

// ServerHost returns the configured external host for this server, defaulting to localhost.
func ServerHost() string {
	host := os.Getenv("DESKTOP_HOST")

	if host != "" {
		return host
	}

	return "localhost"
}
