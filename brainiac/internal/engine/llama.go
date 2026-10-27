package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"brainiac/internal/backend"
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

	mu     sync.Mutex
	cmd    *exec.Cmd
	done   chan struct{}
	client *backend.OpenAI
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
	out, _ := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	helpCache[bin] = string(out)
	return helpCache[bin]
}

func has(help, flag string) bool {
	return strings.Contains(help, flag+" ") || strings.Contains(help, flag+",") || strings.Contains(help, flag+"\n") || strings.Contains(help, flag+"=")
}

// args are the server's flags: the model, a fixed window, a q8_0 KV cache
// with flash attention when the build has them, one slot (its prompt cache
// is what makes the next turn cheap), and loopback only.
func (p *LlamaProc) args(port int) []string {
	help := serverHelp(p.Bin)
	a := []string{"-m", p.Model, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "-c", strconv.Itoa(p.Ctx)}
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
	port, err := freePort()
	if err != nil {
		return err
	}
	cmd := exec.Command(p.Bin, p.args(port)...)
	cmd.Dir = filepath.Dir(p.Bin) // shared libraries sit beside the binary
	var logf *os.File
	if p.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(p.LogPath), 0o755)
		logf, _ = os.Create(p.LogPath)
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("llama-server: %w", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		if logf != nil {
			logf.Close()
		}
		close(done)
	}()
	p.cmd, p.done = cmd, done
	p.client = &backend.OpenAI{Name: "llama-server", BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port), TemplateThinking: true}

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
