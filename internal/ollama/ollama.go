// Package ollama warms the Ollama server every desktop's agent talks to for
// inference, and manages the bundled decurix-ollama container used as a
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

const (
	containerName = ContainerName
	// containerImage is pinned to the release the Windows app installs natively (desktop/electron/wsl/
	// native-ollama.ps1), so every setup runs one reviewed Ollama, not whatever :latest is on the day (C-29). Its
	// digest is the owner's to add once pulled: `docker image inspect --format '{{index .RepoDigests 0}}'`.
	containerImage = "ollama/ollama:0.40.1"
	volumeName     = "decurix_ollama"

	// Model is the Ollama model every desktop agent is configured to use.
	//
	// qwen3:0.6b (this constant's original value) was too weak to actually
	// run the agent loop: tested directly against the real SYSTEM_PROMPT,
	// it echoed the prompt's own placeholder text back verbatim, emitted
	// COMMAND: twice in one response, and then hallucinated a DONE: success
	// reply without ever running anything. qwen2.5-coder:3b replaced it
	// after head-to-head testing against llama3.2:latest (3B) on the
	// prompt's own worked examples - both handled the plain greeting fine,
	// but only qwen2.5-coder:3b replied HELP (as the prompt's own example
	// requires) to an ambiguous "delete the project", where llama3.2
	// hallucinated a destructive `rm -rf` on a guessed path instead.
	//
	// It is devstral-small-2:24b today, for every harness. That was chosen
	// for the browser-agent extension, which has since been removed; nothing
	// in the code needs this model now, and whether to go back to the 3B
	// default is the owner's call (docs/follow-ups.md). It is heavier than
	// the 3B (it needs a GPU that can hold ~15GB); qwen2.5-coder:3b's case
	// above still holds for a CPU-only or memory-constrained host.
	Model = "devstral-small-2:24b"

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

	// BundledURL is where the decurix-ollama container answers on
	// decurix-network (the control plane's; desktops reach it only through
	// the control plane's model proxy). scripts/linux/lib/ollama-setup.sh
	// (sourced by build.sh and rebuild.sh) sets OLLAMA_URL to it for that
	// run only, never in .env, when no other Ollama is reachable from a
	// container.
	BundledURL = "http://decurix-ollama:11434"

	// defaultURL is the Ollama on the host running the control plane, as a
	// desktop container reaches it. See buildDesktopEnv in internal/desktop.
	defaultURL = "http://host.docker.internal:11434"

	// NumCtx is the context window every agent asks this model for:
	// ollama_client.py's num_ctx and TheBot's llm.OllamaProvider default.
	// Ollama loads a model for one context size, and a request for another
	// makes it reload - which on a CPU-only host is minutes, not seconds.
	NumCtx = 8192

	// maxLoadedModels defaults to 1 on memory-constrained machines to prevent
	// concurrent multi-gigabyte runner allocation and memory thrashing.
	// Override with OLLAMA_MAX_LOADED_MODELS in .env if desired.
	maxLoadedModels = "1"

	// DefaultKeepAlive is how long every caller asks Ollama to hold the
	// model after its last request (OLLAMA_KEEP_ALIVE overrides it in each
	// of them). Measured on this repo's own 8 GB Windows host, where the
	// WSL VM is sized to all of it: memory a VM has not touched recently
	// comes back from Windows at ~16 MB/s, so loading the 1.9 GB model
	// cold took 170-230 s (the "runner started in 166 seconds" line in
	// Ollama's log) against 2 s when it was still resident. An hour of
	// 2.2 GB is a better trade than a four-minute first answer.
	DefaultKeepAlive = "60m"
)

// KeepAlive is the keep_alive value every caller sends with a request.
func KeepAlive() string {
	return getenvDefault("OLLAMA_KEEP_ALIVE", DefaultKeepAlive)
}

func defaultMaxLoadedModels() string {
	if v := os.Getenv("OLLAMA_MAX_LOADED_MODELS"); v != "" {
		return v
	}
	return maxLoadedModels
}

// DefaultNumThread returns the recommended thread count for CPU inference,
// strictly reserving at least 1-2 cores for host and desktop responsiveness.
func DefaultNumThread() int {
	if v := getenvInt("OLLAMA_NUM_THREAD", 0); v > 0 {
		return v
	}
	cpus := runtime.NumCPU()
	if cpus > 4 {
		// Leave at least 2 cores completely free for X11, Chromium, VNC, and OS responsiveness
		return cpus - 2
	}
	if cpus > 2 {
		return cpus - 1
	}
	return 1
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// DesktopURL returns the Ollama URL every new desktop is given: OLLAMA_URL
// from the control plane's environment, otherwise the host's own Ollama.
func DesktopURL() string {
	if u := strings.TrimRight(os.Getenv("OLLAMA_URL"), "/"); u != "" {
		return u
	}
	return defaultURL
}

// DesktopModel returns the chat model every new desktop is given:
// OLLAMA_MODEL from the control plane's environment; when that is unset or
// "auto", the model chosen for this machine (see SelectModel); otherwise Model.
func DesktopModel() string {
	if m := pinnedModel(); m != "" {
		return m
	}
	if m, ok := autoModel(); ok {
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

// EnsureReady readies the Ollama the desktops actually use: it chooses the model, and when that Ollama is the bundled
// container it also provisions and starts it and pulls its models. It loads no model (resource plan P1 #7): at boot
// the chat model's weights (1.8 GB on the 8 GB host) were held from the moment the control plane started, whether or
// not anybody opened a desktop that day. WarmAsync loads it the first time something is about to need it - a desktop
// created, viewed or sent a request - so the load still overlaps with what the person is waiting for (a desktop's
// ~20 s boot, the chat opening), which is what kept "hello" to a fresh desktop from timing out. Those triggers, and
// the reload after an eviction, are unchanged.
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

		// Choose before preloading: this is the model the preload loads.
		if err := SelectModel(ctx); err != nil {
			return fmt.Errorf("select model on inference host: %w", err)
		}
		base := dialableURL(ctx, url)
		if os.Getenv("DECURIX_MODEL_AUTOPULL") == "1" {
			if err := ensureRemoteModel(ctx, base, DesktopModel()); err != nil {
				return err
			}
			if err := ensureRemoteModel(ctx, base, EmbedModel); err != nil {
				log.Printf("Could not prepare embedding model: %v", err)
			}
		}
		// No load here: WarmAsync, on first use (see above).

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

		err = drainImagePull(reader)
		reader.Close()

		if err != nil {
			return fmt.Errorf(
				"pull Ollama image: %w",
				err,
			)
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
				"OLLAMA_MAX_LOADED_MODELS=" + defaultMaxLoadedModels(),
				"OLLAMA_FLASH_ATTENTION=" + getenvDefault("OLLAMA_FLASH_ATTENTION", "1"),
				"OLLAMA_KV_CACHE_TYPE=" + getenvDefault("OLLAMA_KV_CACHE_TYPE", "q8_0"),
				"OLLAMA_NUM_PARALLEL=" + getenvDefault("OLLAMA_NUM_PARALLEL", "1"),
				"OLLAMA_NUM_THREAD=" + strconv.Itoa(DefaultNumThread()),
				// Desktops send screens and documents here: no cloud models
				// or web search beside the local ones.
				"OLLAMA_NO_CLOUD=1",
			},

			Labels: map[string]string{
				"decurix":        "true",
				"decurix-ollama": "true",
			},
		}

		threads := DefaultNumThread()
		hostConfig := &container.HostConfig{
			Binds: []string{
				volumeName + ":/root/.ollama",
			},
			Resources: container.Resources{
				NanoCPUs:  int64(threads) * 1_000_000_000,
				CPUShares: 512, // Lower priority than interactive desktop containers (1024)
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

	// The container is up and can say what it has, so this is the moment to
	// choose which chat model it should hold.
	SelectModel(ctx)

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

	// No load here: WarmAsync, on first use (see EnsureReady).
	log.Println("Ollama ready.")

	return nil
}

// drainImagePull reads a Docker image-pull progress stream to its end and
// returns the error the daemon reported inside it, if any. ImagePull only
// starts a pull: a failure mid-download (a dropped connection, a missing
// manifest) arrives as an {"error": ...} message in the stream, not as the
// call's error. The old loop stopped at the first read error and looked at
// nothing, so a failed pull went on to ContainerCreate and failed there as
// "No such image", far from the real cause.
func drainImagePull(r io.Reader) error {
	dec := json.NewDecoder(r)

	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}

		err := dec.Decode(&msg)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read pull progress: %w", err)
		}

		if msg.ErrorDetail.Message != "" {
			return fmt.Errorf("%s", msg.ErrorDetail.Message)
		}
		if msg.Error != "" {
			return fmt.Errorf("%s", msg.Error)
		}
	}
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
func preloadModel(ctx context.Context, baseURL, model string) {
	log.Printf("Preloading Ollama model %s at %s...", model, baseURL)

	// An empty prompt is Ollama's documented way to load a model without
	// generating anything. keep_alive and num_ctx match what the desktop
	// agents send, so the resident copy this creates is the one they go on
	// to use. Without num_ctx it loaded at Ollama's 4096 default, and the
	// agents' first 8192 request threw it away and loaded the model again.
	preloadOpts := map[string]any{
		"num_ctx": NumCtx,
	}
	if nt := getenvInt("OLLAMA_NUM_THREAD", DefaultNumThread()); nt > 0 {
		preloadOpts["num_thread"] = nt
	}
	preloadOpts["num_batch"] = getenvInt("OLLAMA_NUM_BATCH", 512)

	body, err := json.Marshal(map[string]any{
		"model":      model,
		"prompt":     "",
		"keep_alive": KeepAlive(),
		"options":    preloadOpts,
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
		if answers(ctx, BundledURL) {
			return nil
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
	// `ollama show MODEL` exits 0 when the model already exists.
	code, err := m.execWait(ctx, "model check", 200*time.Millisecond, "ollama", "show", name)
	if err != nil {
		return err
	}

	if code == 0 {
		log.Printf("Ollama model %s already exists.", name)
		return nil
	}

	// Model doesn't exist. Pull it.
	log.Printf("Pulling Ollama model %s...", name)

	code, err = m.execWait(ctx, "pull", time.Second, "ollama", "pull", name)
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("Ollama pull failed with exit code %d", code)
	}

	log.Printf("Ollama model %s ready.", name)

	return nil
}

// execWait runs cmd in the Ollama container, checking every poll until it
// exits, and returns its exit code. what names it in an error.
func (m *Manager) execWait(ctx context.Context, what string, poll time.Duration, cmd ...string) (int, error) {
	run, err := m.client.ContainerExecCreate(ctx, containerName, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, fmt.Errorf("create Ollama %s: %w", what, err)
	}

	if err := m.client.ContainerExecStart(ctx, run.ID, container.ExecStartOptions{}); err != nil {
		return 0, fmt.Errorf("start Ollama %s: %w", what, err)
	}

	for {
		result, err := m.client.ContainerExecInspect(ctx, run.ID)
		if err != nil {
			return 0, fmt.Errorf("inspect Ollama %s: %w", what, err)
		}

		if !result.Running {
			return result.ExitCode, nil
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ContainerName is the bundled model server's container, on decurix-network with the control plane. Desktops reach it
// only through the control plane's allow-listed proxy (/api/native-ollama), never on a network of theirs.
const ContainerName = "decurix-ollama"

// warmState guards WarmAsync: at most one warm-up in flight, and at most one
// attempt per warmMinGap however many requests arrive.
var warmState struct {
	mu       sync.Mutex
	enabled  bool
	inflight bool
	last     time.Time
}

// warmMinGap is the shortest time between two warm-up attempts. A load that
// takes minutes must not be started again by the next click.
const warmMinGap = 90 * time.Second

// EnableWarm turns WarmAsync on. It is off until main calls this, so a test
// that reaches a handler which warms the model never dials an Ollama.
func EnableWarm() {
	warmState.mu.Lock()
	warmState.enabled = true
	warmState.mu.Unlock()
}

// warmNeeded, when set, says whether anything will talk to the local model
// at all (SetWarmNeeded). Nil means yes.
var warmNeeded func() bool

// SetWarmNeeded installs the check WarmAsync and main's startup preload ask
// before loading the local model. A control plane whose desktops all run on a
// hosted model (the azure harness) must not spend gigabytes of a small
// machine's memory on a model nothing reads.
func SetWarmNeeded(needed func() bool) {
	warmState.mu.Lock()
	warmNeeded = needed
	warmState.mu.Unlock()
}

// Needed reports whether the local model is wanted (see SetWarmNeeded).
func Needed() bool {
	warmState.mu.Lock()
	needed := warmNeeded
	warmState.mu.Unlock()
	return needed == nil || needed()
}

// WarmAsync makes sure the model desktops use is resident in Ollama, without
// making the caller wait: if it is not loaded, a background load is started
// so that it overlaps with whatever the person is already waiting for - a
// desktop starting (~20 s), the chat opening, a request being delivered.
// Debounced, single-flight and best-effort; a machine with no reachable
// Ollama just logs once per attempt.
func WarmAsync() {
	if !Needed() {
		return
	}

	warmState.mu.Lock()
	if !warmState.enabled || warmState.inflight || time.Since(warmState.last) < warmMinGap {
		warmState.mu.Unlock()
		return
	}
	warmState.inflight = true
	warmState.last = time.Now()
	warmState.mu.Unlock()

	go func() {
		defer func() {
			warmState.mu.Lock()
			warmState.inflight = false
			warmState.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()

		base, model := DesktopURL(), DesktopModel()
		// A resident model is left strictly alone. An earlier version sent
		// it a one-token "touch" to page its weights back in, and measured
		// on the real thing: five chats took 1-4 s each, the touch took 3 s,
		// and the very next chat spent 199 s evaluating its prompt - the
		// touch's two-token prompt had replaced the cached prefix, and
		// re-reading the ~900-token system prompt from scratch costs
		// 75-200 s on this host against ~1 s when cached. Anything that
		// sends the model a request other than an agent's own must not
		// disturb that cache.
		if resident(ctx, base, model) {
			return
		}

		preloadModel(ctx, base, model)
	}()
}

// resident reports whether model is already loaded in the Ollama at baseURL
// at the context size the agents ask for (GET /api/ps). Anything unreadable
// counts as "not resident": loading a model that is already there is
// harmless, skipping one that is not is the cold start this exists to avoid.
func resident(ctx context.Context, baseURL, model string) bool {
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if status, err := getJSON(ctx, baseURL+"/api/ps", 5*time.Second, 1<<20, &ps); err != nil || status != http.StatusOK {
		return false
	}

	want := strings.TrimSuffix(model, ":latest")
	for _, m := range ps.Models {
		got := strings.TrimSuffix(m.Name, ":latest")
		if got != want && strings.TrimSuffix(m.Model, ":latest") != want {
			continue
		}
		return m.ContextLength == 0 || m.ContextLength == NumCtx
	}

	return false
}

// ensureRemoteModel provisions an explicitly opted-in native/remote server
// through its API, without starting a second model server in Linux.
func ensureRemoteModel(ctx context.Context, baseURL, name string) error {
	body, err := json.Marshal(map[string]any{"model": name})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	check := &http.Client{Timeout: 5 * time.Second}
	resp, err := check.Do(req)
	if err != nil {
		return fmt.Errorf("check native Ollama model %s: %w", name, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("check native Ollama model %s: HTTP %d", name, resp.StatusCode)
	}

	log.Printf("Downloading Ollama model %s to %s...", name, baseURL)
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	pull := &http.Client{Timeout: 60 * time.Minute}
	resp, err = pull.Do(req)
	if err != nil {
		return fmt.Errorf("pull native Ollama model %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull native Ollama model %s: HTTP %d", name, resp.StatusCode)
	}
	decoder := json.NewDecoder(resp.Body)
	for {
		var progress struct {
			Status string
			Error  string
		}
		if err := decoder.Decode(&progress); err != nil {
			return fmt.Errorf("pull native Ollama model %s: %w", name, err)
		}
		if progress.Error != "" {
			return fmt.Errorf("pull native Ollama model %s: %s", name, progress.Error)
		}
		if progress.Status == "success" {
			return nil
		}
	}
}
