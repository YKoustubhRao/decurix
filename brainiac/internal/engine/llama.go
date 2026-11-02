package engine

import (
	"context"
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
	Logf        func(format string, a ...any)

	mu     sync.Mutex
	cmd    *exec.Cmd
	done   chan struct{}
	client *backend.OpenAI
	open   []string
	sock   string // the Unix socket's private directory, when one is used
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

// args are the server's flags: the model, a fixed window, a q8_0 KV cache
// with flash attention when the build has them, one slot (its prompt cache
// is what makes the next turn cheap), and loopback or a Unix socket only.
func (p *LlamaProc) args(host string, port int) []string {
	help := serverHelp(p.Bin)
	a := []string{"-m", p.Model, "--host", host, "--port", strconv.Itoa(port), "-c", strconv.Itoa(p.Ctx)}
	if p.Threads > 0 {
		a = append(a, "-t", strconv.Itoa(p.Threads))
	}
	if p.MMProj != "" {
		a = append(a, "--mmproj", p.MMProj)
	}
	if has(help, "--parallel") {
		a = append(a, "--parallel", "1")
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
	return a
}

// Start runs the server and waits until it has loaded the model.
func (p *LlamaProc) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running() {
		return nil
	}
	if p.TemplateSHA != "" {
		got, err := gguf.FileSHA(p.Model)
		if err != nil {
			return fmt.Errorf("llama-server: %w", err)
		}
		if got != p.TemplateSHA {
			return fmt.Errorf("llama-server: %s no longer carries the chat template it was installed with; not serving it. "+
				"After `brainiac update approve-template`, `brainiac pull --latest` installs it again", filepath.Base(p.Model))
		}
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	var logf *os.File
	if p.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(p.LogPath), 0o755)
		logf, _ = os.Create(p.LogPath)
	}
	p.removeSock()
	cmd, client, err := p.launchNoNetwork(port, logf)
	if err != nil {
		p.logf("llama-server: %s cannot run without a network: %v", filepath.Base(p.Model), err)
		p.removeSock()
		if cmd, client, err = p.launchOnLoopback(port, logf); err != nil {
			return fmt.Errorf("llama-server: %w", err)
		}
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
	p.cmd, p.done, p.client = cmd, done, client

	// Loading reads the whole file; on a slow disk under memory pressure that
	// is minutes, so the wait is long, but a crash is noticed at once.
	deadline := time.Now().Add(15 * time.Minute)
	for {
		select {
		case <-done:
			p.cmd = nil
			return fmt.Errorf("llama-server exited while loading %s: %s", filepath.Base(p.Model), tailOf(p.LogPath))
		case <-ctx.Done():
			p.stopLocked()
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ok := p.client.Healthy(hctx)
		cancel()
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			p.stopLocked()
			return errors.New("llama-server did not finish loading in 15 minutes")
		}
	}
}

// command is the server process: none of Brainiac's secrets in its environment.
func (p *LlamaProc) command(host string, port int, logf *os.File) *exec.Cmd {
	cmd := exec.Command(p.Bin, p.args(host, port)...)
	cmd.Dir = filepath.Dir(p.Bin) // shared libraries sit beside the binary
	cmd.Env = isolate.Env()
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	return cmd
}

// policy is the files the server may touch: its build and its model files,
// and the socket's directory when it has one.
func (p *LlamaProc) policy(write ...string) isolate.Policy {
	read := []string{p.Model}
	if p.MMProj != "" {
		read = append(read, p.MMProj)
	}
	return isolate.ForProgram(p.Bin, read, write)
}

func (p *LlamaProc) sandboxes() bool { return !p.NoSandbox && isolate.CanSandbox() }

// launchOnLoopback starts the server on a TCP port, the way that works
// everywhere. Where the kernel can, it still may not connect anywhere, and
// may listen on that one port only. What stays open is named in p.open.
func (p *LlamaProc) launchOnLoopback(port int, logf *os.File) (*exec.Cmd, *backend.OpenAI, error) {
	cmd := p.command("127.0.0.1", port, logf)
	p.open = []string{"network", "files"}
	if p.sandboxes() {
		pol := p.policy()
		pol.BlockTCP, pol.BindTCP = isolate.CanBlockTCP(), []int{port}
		if err := isolate.Wrap(cmd, pol); err != nil {
			p.logf("llama-server: no file sandbox: %v", err)
			cmd = p.command("127.0.0.1", port, logf)
		} else if pol.BlockTCP {
			p.open = []string{"udp"}
		} else {
			p.open = []string{"network"}
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, &backend.OpenAI{Name: "llama-server", BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port), TemplateThinking: true}, nil
}

// launchNoNetwork starts the server with no network at all, reached through a
// Unix socket in a directory only this user can open. It returns an error,
// with nothing started, when this system or build cannot do that.
func (p *LlamaProc) launchNoNetwork(port int, logf *os.File) (*exec.Cmd, *backend.OpenAI, error) {
	if !isolate.CanCutNetwork {
		return nil, nil, errors.New("this system cannot start a process without a network")
	}
	if !strings.Contains(serverHelp(p.Bin), ".sock") {
		return nil, nil, errors.New("this llama-server build has no Unix socket to be reached through")
	}
	dir, err := os.MkdirTemp("", "brainiac-llama-")
	if err != nil {
		return nil, nil, err
	}
	p.sock = dir
	path := filepath.Join(dir, "s.sock")
	if len(path) > 100 { // sun_path holds about 108 bytes
		return nil, nil, fmt.Errorf("the socket path %s is too long", path)
	}
	cmd := p.command(path, port, logf)
	isolate.NoNetwork(cmd)
	p.open = []string{"files"}
	if p.sandboxes() {
		if err := isolate.Wrap(cmd, p.policy(dir)); err != nil {
			p.logf("llama-server: no file sandbox: %v", err)
		} else {
			p.open = nil
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	client := &backend.OpenAI{Name: "llama-server", BaseURL: "http://llama-server", TemplateThinking: true, HTTP: &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}}
	return cmd, client, nil
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

// Open names what a running server can still reach: "files", "network", or
// "udp" when only TCP could be blocked. Nothing for a fully confined one.
func (p *LlamaProc) Open() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running() {
		return nil
	}
	return p.open
}

// Client is the server's API, valid while it runs.
func (p *LlamaProc) Client() *backend.OpenAI {
	p.mu.Lock()
	defer p.mu.Unlock()
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

// Stop kills the server and waits for it to exit.
func (p *LlamaProc) Stop() {
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
	p.cmd = nil
	p.removeSock()
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
