// Package ollama manages the single shared Ollama container that every
// desktop's agent talks to for inference.
package ollama

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

const (
	containerName = "flowera-ollama"
	containerImage = "ollama/ollama:latest"
	volumeName     = "flowera_ollama"

	// Model is the Ollama model every desktop agent is configured to use.
	Model = "qwen3:0.6b"
)

// Manager owns the lifecycle of the shared Ollama container.
type Manager struct {
	client      *client.Client
	networkName string
}

// NewManager creates a Manager backed by the given Docker client, attaching
// the Ollama container to the given Docker network (shared with desktops).
func NewManager(cli *client.Client, networkName string) *Manager {
	return &Manager{client: cli, networkName: networkName}
}

// EnsureReady provisions and starts the shared Ollama container, waits for it
// to become ready, and ensures the configured model is pulled.
func (m *Manager) EnsureReady(ctx context.Context) error {
	// ------------------------------------------------------------
	// Make sure Ollama image exists
	// ------------------------------------------------------------

	_, _, err := m.client.ImageInspectWithRaw(
		ctx,
		containerImage,
	)

	if err != nil {
		log.Printf("Pulling %s...", containerImage)

		reader, err := m.client.ImagePull(
			ctx,
			containerImage,
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
			Name: volumeName,
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
		containerName,
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
				containerName,
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
			Image: containerImage,

			Env: []string{
				"OLLAMA_HOST=0.0.0.0:11434",
			},

			Labels: map[string]string{
				"flowera":        "true",
				"flowera-ollama": "true",
			},
		}

		hostConfig := &container.HostConfig{
			Binds: []string{
				volumeName + ":/root/.ollama",
			},

			RestartPolicy: container.RestartPolicy{
				Name: "unless-stopped",
			},
		}

		networkingConfig := &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				m.networkName: {},
			},
		}

		resp, err := m.client.ContainerCreate(
			ctx,
			config,
			hostConfig,
			networkingConfig,
			nil,
			containerName,
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
		Model,
	)

	if err := m.ensureModel(ctx); err != nil {
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
			"http://flowera-ollama:11434/api/tags",
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

	return fmt.Errorf("timeout waiting for Ollama")
}

// ensureModel pulls the configured Ollama model into the shared Ollama container if it is not already present.
func (m *Manager) ensureModel(ctx context.Context) error {
	// Try `ollama show MODEL`.
	// Exit code 0 means the model already exists.
	checkExec, err := m.client.ContainerExecCreate(
		ctx,
		containerName,
		container.ExecOptions{
			Cmd: []string{
				"ollama",
				"show",
				Model,
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
					Model,
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
		Model,
	)

	pullExec, err := m.client.ContainerExecCreate(
		ctx,
		containerName,
		container.ExecOptions{
			Cmd: []string{
				"ollama",
				"pull",
				Model,
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
		Model,
	)

	return nil
}
