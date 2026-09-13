// Package ollama manages the single shared Ollama container that every
// desktop's agent talks to for inference.
package ollama

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

const (
	containerName  = "flowera-ollama"
	containerImage = "ollama/ollama:latest"
	volumeName     = "flowera_ollama"

	// Model is the Ollama model every desktop agent is configured to use.
	//
	// qwen3:0.6b (this constant's previous value) was too weak to actually
	// run the agent loop: tested directly against the real SYSTEM_PROMPT,
	// it echoed the prompt's own placeholder text back verbatim, emitted
	// COMMAND: twice in one response, and then hallucinated a DONE: success
	// message without ever running anything. qwen2.5-coder:3b was chosen
	// after head-to-head testing against llama3.2:latest (3B) on the
	// prompt's own worked examples: both handled the plain greeting fine,
	// but only qwen2.5-coder:3b replied HELP (as the prompt's own example
	// requires) to an ambiguous "delete the project" - llama3.2 hallucinated
	// a destructive `rm -rf` on a guessed path instead. Still ~1.9GB, so it
	// stays cheap for ensureModel to pull and light enough to run without a
	// GPU, unlike the 7B/14B class models this package does not use.
	Model = "qwen2.5-coder:3b"

	// EmbedModel is the embedding model the thebot harness needs, and it
	// is a genuinely separate dependency: Ollama will not embed with a
	// chat model, so having Model pulled says nothing about whether
	// embedding works.
	//
	// Nothing used to pull it, and the failure was silent in the worst
	// way - soul kept answering, but every write to its Data RAG, its L1
	// long-term memory and its negative-feedback store failed one at a
	// time with `ollama embed returned HTTP 404: model
	// "nomic-embed-text" not found`. An agent that cannot record what it
	// learned still looks like it is working; it just never gets better,
	// and repeats the same failure on the next task.
	EmbedModel = "nomic-embed-text"
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

	// Both models, not just the chat one - see EmbedModel's comment for
	// what silently breaks when the second is missing.
	log.Printf(
		"Ensuring Ollama model %s...",
		Model,
	)

	if err := m.ensureModel(ctx, Model); err != nil {
		return err
	}

	// The embedding model is best-effort, unlike the chat model above.
	// Only the thebot harness ever embeds anything, so failing to get it
	// must not stop ollama- and claude-harness desktops from working -
	// and must not skip the preload below, which is what makes the first
	// message to a new desktop answer in time.
	log.Printf(
		"Ensuring Ollama model %s...",
		EmbedModel,
	)

	if err := m.ensureModel(ctx, EmbedModel); err != nil {
		log.Printf(
			"warning: could not ensure embedding model %s: %v - "+
				"the thebot harness will answer but will not remember "+
				"anything it learns",
			EmbedModel,
			err,
		)
	}

	// Load the chat model before anyone asks a question with it. On a
	// CPU-only host this is tens of seconds of weight loading that would
	// otherwise land on the first message a human sends, inside whatever
	// timeout that harness happens to use - which is exactly how "hello"
	// to a freshly built desktop came back as a request for human help
	// about a deadline being exceeded.
	m.preloadModel(ctx)

	log.Println("Ollama ready.")

	return nil
}

// preloadModel asks Ollama to load the chat model into memory and hold it
// there, so the first real request does not pay for it. Best-effort: a
// failure here only costs the warm start it was trying to buy.
func (m *Manager) preloadModel(ctx context.Context) {
	log.Printf("Preloading Ollama model %s...", Model)

	// An empty prompt is Ollama's documented way to load a model without
	// generating anything. keep_alive matches what the desktop agents
	// send, so the resident copy this creates is the one they go on to
	// use rather than a second load with different settings.
	body := strings.NewReader(
		`{"model":"` + Model + `","prompt":"","keep_alive":"30m"}`,
	)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://flowera-ollama:11434/api/generate",
		body,
	)

	if err != nil {
		log.Printf("Could not build Ollama preload request: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 10 * time.Minute,
	}

	resp, err := client.Do(req)

	if err != nil {
		log.Printf("Ollama preload failed (harmless, just slower): %v", err)
		return
	}

	resp.Body.Close()

	log.Printf("Ollama model %s is loaded and resident.", Model)
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

// ensureModel pulls the named Ollama model into the shared Ollama container if it is not already present.
func (m *Manager) ensureModel(ctx context.Context, name string) error {
	// Try `ollama show MODEL`.
	// Exit code 0 means the model already exists.
	checkExec, err := m.client.ContainerExecCreate(
		ctx,
		containerName,
		container.ExecOptions{
			Cmd: []string{
				"ollama",
				"show",
				name,
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
					name,
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
		name,
	)

	pullExec, err := m.client.ContainerExecCreate(
		ctx,
		containerName,
		container.ExecOptions{
			Cmd: []string{
				"ollama",
				"pull",
				name,
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
		name,
	)

	return nil
}
