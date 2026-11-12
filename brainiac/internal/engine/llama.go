package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/gguf"
	"brainiac/internal/isolate"
)

// LlamaProc is one llama-server process serving one model. Brainiac starts it
// when the model is needed and kills it to give the memory back: a process
// exit is the one unload that always returns every byte.
type LlamaProc struct {
	Bin       string
	Model     string
	MMProj    string
	Ctx       int
	Threads   int
	Embedding bool
	LogPath   string
	// TemplateSHA is the chat template the model was installed with; a file
	// that no longer carries it is not served. Empty for an older lock.
	TemplateSHA string
	NoSandbox   bool // the operator turned the file sandbox off
	// CacheRAMMiB is the host-memory prompt cache the plan counted for this
	// model (--cache-ram); 0 turns it off. llama.cpp's own default, 8 GiB,
	// is never left to apply: nobody would have counted it.
	CacheRAMMiB int
	// Checkpoints caps the context checkpoints per slot (--ctx-checkpoints).
	Checkpoints int
	// OnGPU places every layer on the GPU, as the plan counted it; otherwise
	// none. llama.cpp's automatic fit is off, so the run is what was planned.
	OnGPU bool
	// SlotDir, when set, is where the slot's prompt state is saved before the
	// server stops and restored when it starts again (--slot-save-path), so
	// the next turn after a reload does not read its whole prompt again.
	SlotDir string
	// Spec is the speculative decoding to use: "" none, SpecNgram the
	// build's default n-gram drafting (no second model).
	Spec string
	// Required refuses to start a server that could reach anything beyond
	// its own files and socket (sandbox: required).
	Required bool
	Logf     func(format string, a ...any)

	mu     sync.Mutex
	cmd    *exec.Cmd
	done   chan struct{}
	client *backend.OpenAI // set once the server answers its health check
	open   []string
	sock   string // the Unix socket's private directory, when one is used
	// slotFile is the saved prompt state this server's recipe uses.
	slotFile string
	// loading is closed when a start in progress ends; nil when none is.
	// The lock is not held while a model loads (minutes on a CPU), so
	// status and the other accessors never wait for it.
	loading chan struct{}
}

var (
	helpMu    sync.Mutex
	helpCache = map[string]string{}
)

// serverHelp is llama-server's --help text, read once per binary, so flags
// are only passed to a build that knows them.
func serverHelp(bin string) string {
	helpMu.Lock()
	defer helpMu.Unlock()
	if h, ok := helpCache[bin]; ok {
		return h
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	cmd.Env = isolate.Env()
	out, _ := cmd.CombinedOutput()
	helpCache[bin] = string(out)
	return helpCache[bin]
}

func has(help, flag string) bool {
	return strings.Contains(help, flag+" ") || strings.Contains(help, flag+",") || strings.Contains(help, flag+"\n") || strings.Contains(help, flag+"=")
}

// SpecNgram is n-gram speculative decoding (llama-server --spec-default): it
// drafts from text already in the context and needs no second model. The
// model checks every drafted token, but batched checking is not bit-exact,
// so it is turned on only when checked evals say the answers stay as good.
const SpecNgram = "ngram"

// args are the server's flags: the model, a fixed window, a q8_0 KV cache
// with flash attention when the build has them, one slot (its prompt cache
// is what makes the next turn cheap), and loopback or a Unix socket only.
// Every setting that changes memory or placement is passed explicitly, so a
// new llama.cpp default cannot change what runs: placement (--fit off and
// the GPU layers the plan chose), the prompt cache and the checkpoints.
func (p *LlamaProc) args(host string, port int) []string {
	help := serverHelp(p.Bin)
	a := []string{"-m", p.Model, "--host", host}
	if port > 0 { // a Unix socket needs no port
		a = append(a, "--port", strconv.Itoa(port))
	}
	a = append(a, "-c", strconv.Itoa(p.Ctx))
	if p.Threads > 0 {
		a = append(a, "-t", strconv.Itoa(p.Threads))
	}
	if p.MMProj != "" {
		a = append(a, "--mmproj", p.MMProj)
	}
	if has(help, "--parallel") {
		a = append(a, "--parallel", "1")
	}
	if has(help, "--fit") {
		a = append(a, "--fit", "off")
	}
	if has(help, "--gpu-layers") || has(help, "--n-gpu-layers") {
		ngl := "0"
		if p.OnGPU {
			ngl = "999" // every layer
		}
		a = append(a, "-ngl", ngl)
	}
	if has(help, "--cache-ram") {
		a = append(a, "--cache-ram", strconv.Itoa(max(0, p.CacheRAMMiB)))
	}
	if p.Checkpoints > 0 && has(help, "--ctx-checkpoints") {
		a = append(a, "--ctx-checkpoints", strconv.Itoa(p.Checkpoints))
	}
	if p.Embedding {
		if has(help, "--embeddings") {
			a = append(a, "--embeddings")
		} else {
			a = append(a, "--embedding")
		}
		return a
	}
	if has(help, "--flash-attn") {
		a = append(a, "--flash-attn", "on")
		if has(help, "--cache-type-k") {
			a = append(a, "--cache-type-k", "q8_0", "--cache-type-v", "q8_0")
		}
	}
	if has(help, "--jinja") {
		a = append(a, "--jinja")
	}
	if has(help, "--no-webui") {
		a = append(a, "--no-webui")
	}
	if p.SlotDir != "" && has(help, "--slot-save-path") {
		a = append(a, "--slot-save-path", p.SlotDir)
	}
	if p.Spec == SpecNgram && has(help, "--spec-default") {
		a = append(a, "--spec-default")
	}
	return a
}

// Start runs the server and waits until it has loaded the model. A second
// caller during a load waits for that load instead of starting another.
func (p *LlamaProc) Start(ctx context.Context) error {
	p.mu.Lock()
	for p.loading != nil {
		wait := p.loading
		p.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.mu.Lock()
	}
	if p.running() {
		p.mu.Unlock()
		return nil
	}
	cmd, client, done, err := p.launchLocked()
	if err != nil {
		p.mu.Unlock()
		return err
	}
	loading := make(chan struct{})
	p.loading = loading
	p.mu.Unlock()

	err = p.waitHealthy(ctx, client, done)
	if err == nil {
		p.restoreSlot(ctx, client)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.cmd != cmd:
		if err == nil {
			err = errors.New("llama-server was stopped while it loaded")
		}
	case err != nil:
		p.stopLocked()
	default:
		p.client = client
	}
	p.loading = nil
	close(loading)
	return err
}

// launchLocked starts the server process; p.mu is held.
func (p *LlamaProc) launchLocked() (*exec.Cmd, *backend.OpenAI, chan struct{}, error) {
	if p.TemplateSHA != "" {
		got, err := gguf.FileSHA(p.Model)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("llama-server: %w", err)
		}
		if got != p.TemplateSHA {
			return nil, nil, nil, fmt.Errorf("llama-server: %s no longer carries the chat template it was installed with; not serving it. "+
				"After `brainiac update approve-template`, `brainiac pull` installs it again", filepath.Base(p.Model))
		}
	}
	var logf *os.File
	if p.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(p.LogPath), 0o700)
		logf, _ = os.OpenFile(p.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	}
	p.removeSock()
	l, err := p.launchNoNetwork(logf)
	if err == nil {
		err = p.startLaunch(l)
	}
	if err != nil && !errors.Is(err, errUnconfined) {
		p.logf("llama-server: %s cannot run without a network: %v", filepath.Base(p.Model), err)
		p.removeSock()
		if l, err = p.launchOnLoopback(logf); err == nil {
			err = p.startLaunch(l)
		}
	}
	if err != nil {
		p.removeSock()
		if logf != nil {
			logf.Close()
		}
		return nil, nil, nil, fmt.Errorf("llama-server: %w", err)
	}
	cmd, client := l.cmd, l.client
	p.open = l.open
	if p.SlotDir != "" {
		// Saved state is only ever loaded into the same model, build and
		// settings it came from.
		p.slotFile = "slot-" + fingerprint(p.Bin, fileSig(p.Bin), p.Model, fileSig(p.Model), p.MMProj, fileSig(p.MMProj), p.Ctx, p.OnGPU, p.Spec) + ".bin"
	}
	if len(p.open) > 0 {
		p.logf("llama-server: %s runs with access to: %s", filepath.Base(p.Model), strings.Join(p.open, ", "))
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		if logf != nil {
			logf.Close()
		}
		close(done)
	}()
	p.cmd, p.done, p.client = cmd, done, nil
	return cmd, client, done, nil
}

// waitHealthy waits, without the lock, until the server answers. Loading
// reads the whole file; on a slow disk under memory pressure that is
// minutes, so the wait is long, but a crash is noticed at once.
func (p *LlamaProc) waitHealthy(ctx context.Context, client *backend.OpenAI, done chan struct{}) error {
	deadline := time.Now().Add(15 * time.Minute)
	for {
		select {
		case <-done:
			return fmt.Errorf("llama-server exited while loading %s: %s", filepath.Base(p.Model), tailOf(p.LogPath))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ok := client.Healthy(hctx)
		cancel()
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("llama-server did not finish loading in 15 minutes")
		}
	}
}

// command is the server process: none of Brainiac's secrets in its
// environment, only env (its own key on a TCP port).
func (p *LlamaProc) command(host string, port int, logf *os.File, env ...string) *exec.Cmd {
	cmd := exec.Command(p.Bin, p.args(host, port)...)
	cmd.Dir = buildDir(p.Bin) // where ggml looks for its backends when it cannot read /proc/self/exe
	cmd.Env = isolate.Env(env...)
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	return cmd
}

// policy is the files the server may touch: its program, the libraries it
// loads from its build (ggml's backends among them), its model files, and
// the socket's directory when it has one. ggml finds its backends by
// listing a directory (the working directory, where it cannot read
// /proc/self/exe), so the build may be listed: in a view it holds only
// what is granted.
func (p *LlamaProc) policy(write ...string) isolate.Policy {
	if p.SlotDir != "" {
		write = append(write, p.SlotDir)
	}
	read := []string{p.Model}
	if p.MMProj != "" {
		read = append(read, p.MMProj)
	}
	build := buildDir(p.Bin)
	pol := isolate.ForProgram(p.Bin, read, write, ggmlBackends(build)...)
	pol.List = append(pol.List, build)
	return pol
}

// buildDir is the directory of the real server program, behind any
// symlink: where its build's libraries and backends are.
func buildDir(bin string) string {
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		return filepath.Dir(real)
	}
	return filepath.Dir(bin)
}

// ggmlBackends are the backend modules in a llama.cpp build
// (libggml-cpu-haswell.so, libggml-cuda.so...): ggml opens them itself at
// run time, so no DT_NEEDED names them.
func ggmlBackends(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, "libggml-") && strings.HasSuffix(n, ".so") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}

func (p *LlamaProc) sandboxes() bool { return !p.NoSandbox && isolate.CanSandbox() }

// errUnconfined is a start refused because the sandbox is required and the
// process would not be fully confined.
var errUnconfined = errors.New("the sandbox is required here")

// launch is a server process ready to start, and what it could still reach.
type launch struct {
	cmd    *exec.Cmd
	client *backend.OpenAI
	open   []string
}

// startLaunch starts a prepared server, unless the sandbox is required and
// it would reach more than its own files. It dies with Brainiac.
func (p *LlamaProc) startLaunch(l launch) error {
	if p.Required && len(l.open) > 0 {
		return fmt.Errorf("%w, and %s would still reach: %s; not starting it", errUnconfined, filepath.Base(p.Model), strings.Join(l.open, ", "))
	}
	isolate.DieWithParent(l.cmd)
	return isolate.StartLocked(l.cmd)
}

// launchOnLoopback prepares the server on a TCP port, the way that works
// everywhere. Where the kernel can, it still may not connect anywhere, and
// may listen on that one port only. What stays open is named in open:
// "sockets" always, since only a mount view hides other programs' Unix
// sockets.
func (p *LlamaProc) launchOnLoopback(logf *os.File) (launch, error) {
	port, err := freePort()
	if err != nil {
		return launch{}, err
	}
	// A port on loopback is open to every program here and to any web page
	// the browser opens (llama-server answers every origin by default). So
	// this server gets a key of its own, passed in its environment rather
	// than on its command line, and reflects no origin but localhost's.
	key, err := randomKey()
	if err != nil {
		return launch{}, err
	}
	env := []string{"LLAMA_API_KEY=" + key, "LLAMA_ARG_CORS_ORIGINS=localhost"}
	cmd := p.command("127.0.0.1", port, logf, env...)
	open := []string{"network", "files", "sockets"}
	if p.sandboxes() {
		pol := p.policy()
		pol.BlockTCP, pol.BindTCP = isolate.CanBlockTCP(), []int{port}
		if err := isolate.Wrap(cmd, pol); err != nil {
			p.logf("llama-server: no file sandbox: %v", err)
			cmd = p.command("127.0.0.1", port, logf, env...)
		} else if pol.BlockTCP {
			open = []string{"udp", "sockets"}
		} else {
			open = []string{"network", "sockets"}
		}
	}
	return launch{cmd: cmd, open: open,
		client: &backend.OpenAI{Name: "llama-server", BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port), Key: key, TemplateThinking: true}}, nil
}

// launchNoNetwork prepares the server with no network at all and, unless
// the operator turned the sandbox off, a view of nothing but its build, its
// model and its socket's directory (a mount namespace). It is reached
// through that Unix socket. It returns an error, with nothing started, when
// this system or build cannot do that.
func (p *LlamaProc) launchNoNetwork(logf *os.File) (launch, error) {
	if !isolate.CanCutNetwork {
		return launch{}, errors.New("this system cannot start a process without a network")
	}
	if !strings.Contains(serverHelp(p.Bin), ".sock") {
		return launch{}, errors.New("this llama-server build has no Unix socket to be reached through")
	}
	if !p.NoSandbox {
		if ok, err := isolate.CanConfine(); !ok {
			return launch{}, fmt.Errorf("this system refuses a process its own namespaces: %v", err)
		}
	}
	dir, err := os.MkdirTemp("", "brainiac-llama-")
	if err != nil {
		return launch{}, err
	}
	p.sock = dir
	path := filepath.Join(dir, "s.sock")
	if len(path) > 100 { // sun_path holds about 108 bytes
		return launch{}, fmt.Errorf("the socket path %s is too long", path)
	}
	cmd := p.command(path, 0, logf)
	isolate.NoNetwork(cmd)
	open := []string{"files", "sockets"}
	if !p.NoSandbox {
		if err := isolate.Wrap(cmd, p.policy(dir)); err != nil {
			return launch{}, err
		}
		open = nil
	}
	client := &backend.OpenAI{Name: "llama-server", BaseURL: "http://llama-server", TemplateThinking: true, HTTP: &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}}
	return launch{cmd: cmd, client: client, open: open}, nil
}

func (p *LlamaProc) removeSock() {
	if p.sock != "" {
		_ = os.RemoveAll(p.sock)
		p.sock = ""
	}
}

func (p *LlamaProc) logf(f string, a ...any) {
	if p.Logf != nil {
		p.Logf(f, a...)
	}
}

// Open names what a running server can still reach: "files", "network",
// "udp" when only TCP could be blocked, "sockets" for other programs' Unix
// sockets. Nothing for a fully confined one.
func (p *LlamaProc) Open() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running() {
		return nil
	}
	return p.open
}

// Client is the server's API while it runs, and nil once it has exited (a
// crash or an OOM kill), so a caller starts it again instead of dialing a
// server that is gone.
func (p *LlamaProc) Client() *backend.OpenAI {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running() {
		return nil
	}
	return p.client
}

// Running reports whether the process is alive.
func (p *LlamaProc) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running()
}

func (p *LlamaProc) running() bool {
	if p.cmd == nil || p.done == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// saveSlot writes the slot's prompt state to disk before the server goes,
// so the conversation it held is not read from its start after a reload.
// It is only a saving: a failure costs a slower next turn and is logged.
func (p *LlamaProc) saveSlot() {
	p.mu.Lock()
	file, running := p.slotFile, p.running() && p.loading == nil
	c := p.client
	p.mu.Unlock()
	if p.SlotDir == "" || file == "" || !running || c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.SlotAction(ctx, 0, "save", file); err != nil {
		p.logf("llama-server: could not save %s's prompt state: %v", filepath.Base(p.Model), err)
		_ = os.Remove(filepath.Join(p.SlotDir, file))
	}
}

// restoreSlot loads the prompt state saved for this recipe, if there is one.
// A file that will not load is removed: the next turn reads its prompt.
func (p *LlamaProc) restoreSlot(ctx context.Context, c *backend.OpenAI) {
	p.mu.Lock()
	file := p.slotFile
	p.mu.Unlock()
	if p.SlotDir == "" || file == "" {
		return
	}
	path := filepath.Join(p.SlotDir, file)
	if _, err := os.Stat(path); err != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := c.SlotAction(rctx, 0, "restore", file); err != nil {
		p.logf("llama-server: could not restore %s's prompt state, it starts empty: %v", filepath.Base(p.Model), err)
		_ = os.Remove(path)
	}
}

// Stop saves the slot's prompt state (when SlotDir is set), kills the
// server and waits for it to exit.
func (p *LlamaProc) Stop() {
	p.saveSlot()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *LlamaProc) stopLocked() {
	if p.cmd != nil && p.cmd.Process != nil && p.running() {
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
		}
	}
	p.cmd, p.client = nil, nil
	p.removeSock()
}

// randomKey is a fresh secret for one server's API.
func randomKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// tailOf is the last lines of a log file, for an error message.
func tailOf(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, " | ")
}
