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

	desktopImageBasic     = "flowera-desktop:basic"
	desktopImageDeveloper = "flowera-desktop:developer"
	desktopImageAI        = "flowera-desktop:ai"

	// NetworkName is the shared Docker bridge network desktop containers
	// (and the shared Ollama container) are attached to.
	NetworkName = "flowera-network"

	// Agent harnesses: which reasoning engine drives a desktop's tasks.
	HarnessOllama = "ollama"
	HarnessClaude = "claude"
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

	env := []string{
		"VNC_PW=" + defaultPassword,
		"VNC_RESOLUTION=1280x720",
		"DESKTOP_ID=" + strings.ToLower(
			strings.ReplaceAll(name, " ", "-"),
		),
		"DESKTOP_NAME=" + name,
		"CONTROL_PLANE_URL=http://host.docker.internal:8080",
		"OLLAMA_URL=http://flowera-ollama:11434",
		"OLLAMA_MODEL=" + ollama.Model,
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
