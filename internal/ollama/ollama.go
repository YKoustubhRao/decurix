// Package ollama warms the Ollama server every desktop's agent talks to for
// inference, and manages the bundled flowera-ollama container used as a
// fallback when no other server is reachable.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
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
	// reply without ever running anything. qwen2.5-coder:3b was chosen
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

	// BundledURL is where the flowera-ollama container answers from
	// anything on flowera-network. scripts/linux/build.sh writes it into
	// OLLAMA_URL only when no other Ollama is reachable from a container.
	BundledURL = "http://flowera-ollama:11434"

	// defaultURL is the Ollama on the host running the control plane, as a
	// desktop container reaches it. See buildDesktopEnv in internal/desktop.
	defaultURL = "http://host.docker.internal:11434"

	// NumCtx is the context window every agent asks this model for:
	// ollama_client.py's num_ctx and TheBot's llm.OllamaProvider default.
	// Ollama loads a model for one context size, and a request for another
	// makes it reload - which on a CPU-only host is minutes, not seconds.
	NumCtx = 8192

	// maxLoadedModels raises Ollama's own ceiling (OLLAMA_MAX_LOADED_MODELS,
	// server default 1 on a CPU-only host) on the bundled container from 1
	// to 2 - exactly enough for the two context sizes actually in play
	// system-wide (NumCtx for every ordinary call, TheBot coding's 32768,
	// see internal/coding.codeGenNumCtx), never more. Below this, the same
	// model at two different num_ctx values is two different Ollama
	// runners that take turns evicting each other: measured, a single
	// coding call reloaded the shared model for every desktop (161s) and
	// then reloaded it back the moment an ordinary chat turn followed it.
	// Raising the ceiling lets both stay resident; codeGenKeepAlive (a
	// short 5m, against the chat runner's 30m) is what then gives the
	// rarely-used one's memory back promptly instead of both simply
	// staying loaded forever. Only takes effect for the bundled container -
	// an external Ollama named by OLLAMA_URL is not this control plane's to
	// configure, and needs the same env var set by hand for the same fix
	// to apply there.
	maxLoadedModels = "2"
)

// DesktopURL returns the Ollama URL every new desktop is given: OLLAMA_URL
// from the control plane's environment, otherwise the host's own Ollama.
func DesktopURL() string {
	if u := strings.TrimRight(os.Getenv("OLLAMA_URL"), "/"); u != "" {
		return u
	}
	return defaultURL
}

// DesktopModel returns the chat model every new desktop is given:
// OLLAMA_MODEL from the control plane's environment, otherwise Model.
func DesktopModel() string {
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		return m
	}
	return Model
}

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

// EnsureReady warms the Ollama the desktops actually use. When that is the
// bundled container, it also provisions and starts it and pulls its models.
func (m *Manager) EnsureReady(ctx context.Context) error {
	// The bundled container is only a fallback, and desktops reach
	// whatever OLLAMA_URL names. This used to start the bundled container
	// and preload the model into it regardless - measured on a 7.6 GB
	// CPU-only host whose desktops all used the host's own Ollama: the
	// preload took 2GB of RAM on a server nobody talked to, while the
	// server they did talk to loaded cold, in 210 seconds, inside the first
	// request's 300-second timeout. That request came back as a query
	// saying the model did not answer in time.
	if url := DesktopURL(); url != BundledURL {
		log.Printf(
			"Desktops use the Ollama at %s - leaving the bundled %s container alone.",
			url,
			containerName,
		)

		m.preloadModel(ctx, dialableURL(ctx, url), DesktopModel())

		return nil
	}

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
				"OLLAMA_MAX_LOADED_MODELS=" + maxLoadedModels,
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
		DesktopModel(),
	)

	if err := m.ensureModel(ctx, DesktopModel()); err != nil {
		return err
	}

	// The embedding model is best-effort, unlike the chat model above.
	// Only the thebot harness ever embeds anything, so failing to get it
	// must not stop ollama- and claude-harness desktops from working -
	// and must not skip the preload below, which is what makes the first
	// request to a new desktop answer in time.
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
	// otherwise land on the first request a human sends, inside whatever
	// timeout that harness happens to use - which is exactly how "hello"
	// to a freshly built desktop came back as a query about a deadline
	// being exceeded.
	m.preloadModel(ctx, BundledURL, DesktopModel())

	log.Println("Ollama ready.")

	return nil
}

// dialableURL rewrites a desktop-facing Ollama URL into one this process
// can reach. host.docker.internal is how a container reaches the host; the
// control plane under docker compose has the same entry, but the standalone
// binary runs on the host itself, where that host is simply loopback.
// Whether the name resolves proves nothing: on a WSL host it resolves to
// the Windows side's address and the connection times out.
func dialableURL(ctx context.Context, raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil || u.Hostname() != "host.docker.internal" || answers(ctx, raw) {
		return raw
	}

	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	} else {
		u.Host = "127.0.0.1"
	}

	return u.String()
}

// answers reports whether an Ollama API responds at baseURL within a few
// seconds.
func answers(ctx context.Context, baseURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// preloadModel asks the Ollama at baseURL to load model into memory and
// hold it there, so the first real request does not pay for it.
// Best-effort: a failure here only costs the warm start it was trying to
// buy.
func (m *Manager) preloadModel(ctx context.Context, baseURL, model string) {
	log.Printf("Preloading Ollama model %s at %s...", model, baseURL)

	// An empty prompt is Ollama's documented way to load a model without
	// generating anything. keep_alive and num_ctx match what the desktop
	// agents send, so the resident copy this creates is the one they go on
	// to use. Without num_ctx it loaded at Ollama's 4096 default, and the
	// agents' first 8192 request threw it away and loaded the model again.
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"prompt":     "",
		"keep_alive": "30m",
		"options":    map[string]any{"num_ctx": NumCtx},
	})

	if err != nil {
		log.Printf("Could not build Ollama preload request: %v", err)
		return
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+"/api/generate",
		bytes.NewReader(body),
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

	defer resp.Body.Close()

	// A missing model is a clean HTTP 404, which this used to report as
	// "loaded and resident".
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf(
			"Ollama preload of %s failed (harmless, just slower): HTTP %d: %s",
			model,
			resp.StatusCode,
			strings.TrimSpace(string(detail)),
		)
		return
	}

	log.Printf("Ollama model %s is loaded and resident.", model)
}

// waitForOllama polls the Ollama API until it responds successfully, the context is cancelled, or a two-minute deadline elapses.
func waitForOllama(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)

	for time.Now().Before(deadline) {

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			BundledURL+"/api/tags",
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
