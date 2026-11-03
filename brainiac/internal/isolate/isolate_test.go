package isolate

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestFilterKeepsOnlyWhatAProcessNeeds(t *testing.T) {
	got := strings.Join(filter([]string{"PATH=/bin", "AZURE_KEY=secret", "HF_TOKEN=secret", "BRAINIAC_API=secret",
		"OMP_NUM_THREADS=4", "CUDA_VISIBLE_DEVICES=0", "ANTHROPIC_API_KEY=secret", "MY_PATH=/x", "broken"}, false), " ")
	if got != "PATH=/bin OMP_NUM_THREADS=4 CUDA_VISIBLE_DEVICES=0" {
		t.Fatal(got)
	}
}

func TestFilterFoldsCaseOnlyOnWindows(t *testing.T) {
	if got := filter([]string{"Path=C:\\x", "SystemRoot=C:\\w"}, true); len(got) != 2 {
		t.Fatalf("windows: %v", got)
	}
	if got := filter([]string{"Path=/x"}, false); len(got) != 0 {
		t.Fatalf("elsewhere: %v", got)
	}
}

func TestEnvDropsSecretsAndAddsExtra(t *testing.T) {
	t.Setenv("AZURE_KEY", "secret")
	got := strings.Join(Env("EXTRA=1"), "\n")
	if strings.Contains(got, "secret") || !strings.Contains(got, "EXTRA=1") {
		t.Fatal(got)
	}
}

// The test binary runs itself as the isolated process: a real child, trying a
// real connection to a listener its parent holds open on loopback.
func TestNoNetworkCutsLoopbackForARealProcess(t *testing.T) {
	if addr := os.Getenv("BRAINIAC_ISOLATE_DIAL"); addr != "" {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(7)
		}
		c.Close()
		os.Exit(0)
	}
	if !CanCutNetwork {
		t.Skip("no network namespace on this system")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	run := func(cut bool) error {
		cmd := exec.Command(os.Args[0], "-test.run=TestNoNetworkCutsLoopbackForARealProcess")
		cmd.Env = Env("BRAINIAC_ISOLATE_DIAL=" + l.Addr().String())
		if cut {
			NoNetwork(cmd)
		}
		return cmd.Run()
	}
	if err := run(false); err != nil {
		t.Fatalf("without isolation the child should connect: %v", err)
	}
	err = run(true)
	var ee *exec.ExitError
	if err == nil {
		t.Fatal("the isolated child reached the listener")
	}
	if !asExit(err, &ee) {
		t.Skipf("this system refuses network namespaces: %v", err)
	}
	if ee.ExitCode() != 7 {
		t.Fatalf("the child failed some other way: %v", err)
	}
}

// The child runs as a real sandboxed process and reports what it could do
// through its exit code: 0 everything as expected, otherwise which check failed.
func TestSandboxConfinesARealProcess(t *testing.T) {
	if os.Getenv("BRAINIAC_SANDBOX_CHILD") == "1" {
		sandboxChild()
	}
	if !CanSandbox() {
		t.Skip("no Landlock on this system")
	}
	allowed, secret, writable := t.TempDir(), t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(allowed, "model"), "weights")
	mustWrite(t, filepath.Join(secret, "key"), "secret")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=TestSandboxConfinesARealProcess")
	cmd.Env = Env("BRAINIAC_SANDBOX_CHILD=1", "CHILD_ALLOWED="+filepath.Join(allowed, "model"), "CHILD_SECRET="+filepath.Join(secret, "key"),
		"CHILD_WRITABLE="+writable, "CHILD_PARENT="+strconv.Itoa(os.Getpid()))
	if err := Wrap(cmd, ForProgram(self, []string{filepath.Join(allowed, "model")}, []string{writable})); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func sandboxChild() {
	fail := func(code int, what string, err error) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
		os.Exit(code)
	}
	if _, err := os.ReadFile(os.Getenv("CHILD_ALLOWED")); err != nil {
		fail(11, "could not read the allowed file", err)
	}
	if _, err := os.ReadFile("/proc/cpuinfo"); err != nil {
		fail(12, "could not read the machine's facts", err)
	}
	f := filepath.Join(os.Getenv("CHILD_WRITABLE"), "x")
	if err := os.WriteFile(f, []byte("ok"), 0o600); err != nil {
		fail(13, "could not write where it may", err)
	}
	if _, err := os.ReadFile(os.Getenv("CHILD_SECRET")); err == nil {
		fail(21, "read a file outside its policy", nil)
	}
	if _, err := os.ReadFile("/proc/" + os.Getenv("CHILD_PARENT") + "/environ"); err == nil {
		fail(22, "read its parent's environment", nil)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(os.Getenv("CHILD_SECRET")), "y"), []byte("no"), 0o600); err == nil {
		fail(23, "wrote outside its policy", nil)
	}
	if err := exec.Command("/bin/sh", "-c", "cat \"$CHILD_SECRET\"").Run(); err == nil {
		fail(24, "a child of the sandbox read the secret", nil)
	}
	os.Exit(0)
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Blocking TCP: the listening port it was given works, connecting anywhere does not.
func TestSandboxBlocksTCPButLetsItListen(t *testing.T) {
	if addr := os.Getenv("BRAINIAC_TCP_CHILD_DIAL"); addr != "" {
		if _, err := net.Dial("tcp", addr); err == nil {
			os.Exit(31)
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("BRAINIAC_TCP_CHILD_PORT"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(32)
		}
		l.Close()
		os.Exit(0)
	}
	if !CanBlockTCP() {
		t.Skip("this kernel's Landlock cannot block TCP")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	self, _ := os.Executable()
	cmd := exec.Command(self, "-test.run=TestSandboxBlocksTCPButLetsItListen")
	cmd.Env = Env("BRAINIAC_TCP_CHILD_DIAL="+l.Addr().String(), "BRAINIAC_TCP_CHILD_PORT="+strconv.Itoa(port))
	p := ForProgram(self, nil, nil)
	p.BlockTCP, p.BindTCP = true, []int{port}
	if err := Wrap(cmd, p); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func asExit(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// lockedBuffer collects a child's output from two pipes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
