package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/isolate"
)

func writeGGUF(t *testing.T, template string) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("GGUF")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	binary.Write(&b, binary.LittleEndian, uint64(0))
	binary.Write(&b, binary.LittleEndian, uint64(1))
	key := "tokenizer.chat_template"
	binary.Write(&b, binary.LittleEndian, uint64(len(key)))
	b.WriteString(key)
	binary.Write(&b, binary.LittleEndian, uint32(8))
	binary.Write(&b, binary.LittleEndian, uint64(len(template)))
	b.WriteString(template)
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The refusal comes before any process: a file changed on disk after install
// must never reach the server. The binary here does not exist.
func TestAFileThatLostItsInstalledTemplateIsNotServed(t *testing.T) {
	p := &LlamaProc{Bin: filepath.Join(t.TempDir(), "no-such-server"), Model: writeGGUF(t, "{{ changed }}"), TemplateSHA: "abc"}
	err := p.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "chat template") {
		t.Fatalf("%v", err)
	}
	if p.Running() {
		t.Fatal("it started")
	}
}

// A real llama-server when one is named, with a real small model: it must
// answer through its socket, hold no secret, and be unable to reach out.
// BRAINIAC_LIVE_LLAMA is the binary, BRAINIAC_LIVE_GGUF the model.
func TestLiveServerRunsWithNoNetworkAndNoSecrets(t *testing.T) {
	bin, model := os.Getenv("BRAINIAC_LIVE_LLAMA"), os.Getenv("BRAINIAC_LIVE_GGUF")
	if bin == "" || model == "" {
		t.Skip("set BRAINIAC_LIVE_LLAMA and BRAINIAC_LIVE_GGUF to run a real server")
	}
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces are Linux only")
	}
	t.Setenv("AZURE_KEY", "secret-that-must-not-leak")
	var logged []string
	p := &LlamaProc{Bin: bin, Model: model, Ctx: 256, Threads: 1, LogPath: filepath.Join(t.TempDir(), "llama.log"),
		Logf: func(f string, a ...any) { logged = append(logged, f) }}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if open := p.Open(); len(open) != 0 {
		t.Fatalf("the server can still reach %v; %v", open, logged)
	}
	sock := p.sock
	if _, err := os.Stat(filepath.Join(sock, "s.sock")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory %v %v", fi.Mode(), err)
	}
	res, err := p.Client().Chat(ctx, backend.ChatRequest{MaxTokens: 4, Messages: []backend.Message{{Role: "user", Text: "Once"}}})
	if err != nil {
		t.Fatalf("no answer through the socket: %v", err)
	}
	t.Logf("answered %q", res.Text)
	pid := p.cmd.Process.Pid
	env, err := os.ReadFile(filepathProc(pid, "environ"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env, []byte("secret-that-must-not-leak")) {
		t.Fatal("the secret is in the server's environment")
	}
	// It became the real server, under a sandbox it cannot shed.
	status, err := os.ReadFile(filepathProc(pid, "status"))
	if err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1") {
		t.Fatalf("no sandbox on the running server: %v\n%s", err, status)
	}
	if exe, _ := os.Readlink(filepathProc(pid, "exe")); !strings.Contains(exe, "llama-server") {
		t.Fatalf("the process is %s, not the server", exe)
	}
	// Its namespace differs from ours, and has no interface but a loopback that is down.
	ours, _ := os.Readlink("/proc/self/ns/net")
	theirs, err := os.Readlink(filepathProc(pid, "ns/net"))
	if err != nil || ours == theirs {
		t.Fatalf("same network namespace: %s %s %v", ours, theirs, err)
	}
	route, err := os.ReadFile(filepathProc(pid, "net/route"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(route)), "\n"); len(lines) > 1 {
		t.Fatalf("the server has routes: %s", route)
	}
	if out, err := exec.Command("nsenter", "-U", "-n", "--preserve-credentials", "-t", strconv.Itoa(pid), "cat", "/proc/net/dev").CombinedOutput(); err == nil {
		t.Logf("its interfaces:\n%s", out)
	}
	p.Stop()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("the socket directory is still there: %v", err)
	}
}

// A build with no socket support falls back to loopback, and says so: the
// model still serves, and status names it as having a network.
func TestLiveServerWithoutASocketFallsBackAndSaysSo(t *testing.T) {
	bin, model := os.Getenv("BRAINIAC_LIVE_LLAMA"), os.Getenv("BRAINIAC_LIVE_GGUF")
	if bin == "" || model == "" || runtime.GOOS != "linux" {
		t.Skip("set BRAINIAC_LIVE_LLAMA and BRAINIAC_LIVE_GGUF to run a real server on Linux")
	}
	// A wrapper that hides the socket from --help. The sandbox grants a
	// server's own file and libraries, not the wrapper's target, so this
	// runs with the file sandbox off: what is checked is the fallback.
	old := filepath.Join(filepath.Dir(bin), "old-server")
	script := "#!/bin/sh\n[ \"$1\" = --help ] && exit 0\nexec \"" + bin + "\" \"$@\"\n"
	if err := os.WriteFile(old, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(old) })
	t.Setenv("AZURE_KEY", "secret-that-must-not-leak")
	var logged []string
	p := &LlamaProc{Bin: old, Model: model, Ctx: 256, Threads: 1, LogPath: filepath.Join(t.TempDir(), "llama.log"), NoSandbox: true,
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if len(p.Open()) == 0 || !strings.Contains(strings.Join(logged, " "), "cannot run without a network") {
		t.Fatalf("open %v, logged %v", p.Open(), logged)
	}
	t.Logf("still open: %v", p.Open())
	if _, err := p.Client().Chat(ctx, backend.ChatRequest{MaxTokens: 4, Messages: []backend.Message{{Role: "user", Text: "Once"}}}); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepathProc(p.cmd.Process.Pid, "environ"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env, []byte("secret-that-must-not-leak")) {
		t.Fatal("the secret is in the server's environment")
	}
}

// The fallback where the host refuses namespaces: llama-server on a TCP
// port under Landlock alone. Its libraries and ggml's backends are granted
// by file there too, so it must still load and answer, with its file
// sandbox on ("files" not open), no secret and no new privileges.
func TestLiveServerOnLoopbackUnderLandlockAlone(t *testing.T) {
	bin, model := os.Getenv("BRAINIAC_LIVE_LLAMA"), os.Getenv("BRAINIAC_LIVE_GGUF")
	if bin == "" || model == "" || runtime.GOOS != "linux" {
		t.Skip("set BRAINIAC_LIVE_LLAMA and BRAINIAC_LIVE_GGUF to run a real server on Linux")
	}
	if !isolate.CanSandbox() {
		t.Skip("no Landlock on this kernel")
	}
	t.Setenv("AZURE_KEY", "secret-that-must-not-leak")
	p := &LlamaProc{Bin: bin, Model: model, Ctx: 256, Threads: 1, LogPath: filepath.Join(t.TempDir(), "llama.log")}
	logf, err := os.Create(p.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	l, err := p.launchOnLoopback(logf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(l.open, " "), "files") {
		t.Fatalf("no file sandbox: still open %v", l.open)
	}
	if err := p.startLaunch(l); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = l.cmd.Wait(); close(done) }()
	defer func() { _ = l.cmd.Process.Kill(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := p.waitHealthy(ctx, l.client, done); err != nil {
		t.Fatal(err)
	}
	res, err := l.client.Chat(ctx, backend.ChatRequest{MaxTokens: 4, Messages: []backend.Message{{Role: "user", Text: "Once"}}})
	if err != nil {
		t.Fatalf("no answer on loopback: %v", err)
	}
	pid := l.cmd.Process.Pid
	env, err := os.ReadFile(filepathProc(pid, "environ"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env, []byte("secret-that-must-not-leak")) {
		t.Fatal("the secret is in the server's environment")
	}
	if status, err := os.ReadFile(filepathProc(pid, "status")); err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1") {
		t.Fatalf("no sandbox on the running server: %v\n%s", err, status)
	}
	t.Logf("answered %q; still open: %v", res.Text, l.open)
}

func filepathProc(pid int, name string) string { return "/proc/" + strconv.Itoa(pid) + "/" + name }

// The server's policy grants ggml's backends, which ggml opens itself and
// no DT_NEEDED names, and lets it list its build to find them. Other
// programs' libraries and other files are not backends. The files here are
// not ELF, so on Linux the backends are reached through the fallback (the
// build read-only); isolate's tests check the per-file grant.
func TestThePolicyGrantsTheGGMLBackendsAndListsTheBuild(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"llama-server", "libggml-cpu-haswell.so", "libggml-cuda.so", "libllama-cli-impl.so", "libggml-base.so.0", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{filepath.Join(dir, "libggml-cpu-haswell.so"), filepath.Join(dir, "libggml-cuda.so")}
	if got := ggmlBackends(dir); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("backends %v, want %v", got, want)
	}
	p := &LlamaProc{Bin: filepath.Join(dir, "llama-server"), Model: filepath.Join(dir, "m.gguf")}
	pol := p.policy()
	if strings.Join(pol.List, "|") != dir {
		t.Fatalf("list %v", pol.List)
	}
	read := "|" + strings.Join(pol.Read, "|") + "|"
	for _, w := range want {
		if !strings.Contains(read, "|"+w+"|") && !strings.Contains(read, "|"+dir+"|") {
			t.Fatalf("%s not granted: %v", w, pol.Read)
		}
	}
}

// With real ELF files, each backend that is a shared object is granted by
// itself, and the build's directory is not.
func TestThePolicyGrantsEachGGMLBackendNotTheBuild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the per-file grant is Linux's")
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, data, 0o755); err != nil { // an ELF program with nothing of its own
		t.Fatal(err)
	}
	// A shared object every Linux has stands in for a backend.
	var lib string
	for _, c := range []string{"/lib/x86_64-linux-gnu/libc.so.6", "/lib64/libc.so.6", "/lib/aarch64-linux-gnu/libc.so.6", "/usr/lib/libc.so.6"} {
		if _, err := os.Stat(c); err == nil {
			lib = c
			break
		}
	}
	if lib == "" {
		t.Skip("no libc.so.6 found to stand in for a backend")
	}
	so, err := os.ReadFile(lib)
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(dir, "libggml-cpu-x64.so")
	if err := os.WriteFile(backend, so, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libggml-bad.so"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	pol := (&LlamaProc{Bin: bin, Model: filepath.Join(dir, "m.gguf")}).policy()
	read := "|" + strings.Join(pol.Read, "|") + "|"
	if !strings.Contains(read, "|"+backend+"|") || strings.Contains(read, "|"+dir+"|") || strings.Contains(read, "libggml-bad.so") {
		t.Fatalf("read %v", pol.Read)
	}
}
