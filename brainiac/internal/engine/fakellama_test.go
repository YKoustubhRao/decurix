package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/install"
	"brainiac/internal/selector"
)

// TestMain lets the test binary stand in for llama-server: started with
// llama-server's flags, it serves /health and a chat answer. What it does is
// read from the -m file, so a test needs no real model: "die-after-health"
// exits once its first health check is answered (an OOM kill after loading),
// the first time it runs beside a given counter file.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "--help" || os.Args[1] == "-m") {
		fakeLlama(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func fakeLlama(args []string) {
	if args[0] == "--help" {
		os.Exit(0) // knows no optional flags, and no Unix socket
	}
	var model, host, port string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-m":
			model = args[i+1]
		case "--host":
			host = args[i+1]
		case "--port":
			port = args[i+1]
		}
	}
	spec, _ := os.ReadFile(model)
	die := false
	if strings.TrimSpace(string(spec)) == "die-after-health" {
		counter := model + ".starts"
		n, _ := os.ReadFile(counter)
		starts, _ := strconv.Atoi(string(n))
		starts++
		_ = os.WriteFile(counter, []byte(strconv.Itoa(starts)), 0o600)
		die = starts == 1
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		os.Exit(3)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if die {
			w.(http.Flusher).Flush()
			go func() { time.Sleep(50 * time.Millisecond); os.Exit(9) }()
		}
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"fake","choices":[{"message":{"content":"answer from the fake server"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	})
	_ = http.Serve(l, mux)
	os.Exit(0)
}

// llamaEngine is an engine whose brain is the fake llama-server.
func llamaEngine(t *testing.T, spec string) (*Engine, *unit) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(model, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	src := catalog.Source{Kind: catalog.SourceHF, Runtime: catalog.RuntimeLlamaCpp, Repo: "fake/fake", Quant: "Q4"}
	pick := selector.Pick{Role: catalog.RoleBrain, Model: "fake", Pool: "host", MemGB: 0.1, Source: src}
	lock := &install.Lock{LlamaServer: self, Plan: &selector.Plan{Picks: []selector.Pick{pick}, Budgets: []selector.Budget{{Pool: "host", BudgetGB: 100}}},
		Units: map[string]install.Unit{install.UnitKey("fake", src): {Files: map[string]string{"model": model}}}}
	cfg := config.Default()
	cfg.Mode, cfg.Sandbox = config.ModeLocal, config.SandboxOff
	e := newEngine(t, cfg, lock)
	t.Cleanup(e.Close)
	return e, e.byRole[catalog.RoleBrain]
}

// A llama-server that dies after loading (an OOM kill) is loaded again on
// the next request, instead of every request failing until a restart.
func TestADeadModelServerIsLoadedAgain(t *testing.T) {
	e, u := llamaEngine(t, "die-after-health")
	release, _, err := e.res.Acquire(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	release()
	deadline := time.Now().Add(10 * time.Second)
	for u.llama.Running() {
		if time.Now().After(deadline) {
			t.Fatal("the fake server did not die")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if u.llama.Client() != nil {
		t.Fatal("a dead server still hands out its client")
	}
	res, err := e.Chat(context.Background(), Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "hello"}}}}})
	if err != nil {
		t.Fatalf("the request after the crash failed: %v", err)
	}
	if res.Text != "answer from the fake server" || !res.Complete {
		t.Fatalf("res %+v", res)
	}
	var loaded bool
	for _, a := range res.Trace.Atoms {
		if a.Op == "brain" && a.LoadMs > 0 {
			loaded = true
		}
	}
	if !loaded {
		t.Fatal("the dead server was not loaded again through the residency manager")
	}
}

// A unit that died is forgotten and loaded again; a hold on the dead one,
// let go later, does not release the new one.
func TestResidencyReloadsADeadUnit(t *testing.T) {
	r := NewResidency(map[string]float64{"host": 3}, nil)
	a := &fakeUnit{name: "a", gb: 1}
	relOld, _, err := r.Acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	a.dead = true
	relNew, _, err := r.Acquire(context.Background(), a)
	if err != nil || a.loads != 2 {
		t.Fatalf("a dead unit was not loaded again: loads %d err %v", a.loads, err)
	}
	a.dead = false
	relOld() // the hold on the dead process
	if e := r.loaded["a"]; e == nil || e.refs != 1 {
		t.Fatalf("the old hold released the new load: %+v", e)
	}
	relNew()
}

// With sandbox: required, a model server that could not be fully confined
// is refused, not started. The fake server has no Unix socket, so it could
// only run on loopback, which is never fully confined.
func TestARequiredSandboxRefusesAnUnconfinedServer(t *testing.T) {
	e, u := llamaEngine(t, "die-after-health")
	u.llama.NoSandbox, u.llama.Required = false, true
	_, _, err := e.res.Acquire(context.Background(), u)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("an unconfined server started: %v", err)
	}
	if _, err := os.Stat(u.llama.Model + ".starts"); err == nil || u.llama.Running() {
		t.Fatal("the server process ran")
	}
}

func TestOnlyAnOrphanedServerIsSwept(t *testing.T) {
	args := []string{"/store/bin/llama-server", "-m", "m.gguf", "--host", "/tmp/brainiac-llama-123/s.sock"}
	if !orphanServer(args, "systemd") || orphanServer(args, "brainiac") || orphanServer([]string{"llama-server", "--host", "127.0.0.1"}, "systemd") {
		t.Fatal("orphanServer")
	}
}
