package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker records the housekeeping docker commands a hop runs and answers them.
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	ps    string
	fail  bool
}

func (f *fakeDocker) install(t *testing.T) {
	t.Helper()
	saved := hopExec
	hopExec = func(ctx context.Context, dockerBin string, args ...string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, append([]string{dockerBin}, args...))
		if args[0] == "ps" {
			return []byte(f.ps), nil
		}
		if f.fail {
			return []byte("Error: No such container: x"), errors.New("exit status 1")
		}
		return nil, nil
	}
	t.Cleanup(func() { hopExec = saved })
}

func (f *fakeDocker) removed() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) > 2 && c[1] == "rm" && c[2] == "-f" {
			out = append(out, c[3:])
		}
	}
	return out
}

func TestAHopIsNamedLabelledAndRemovedWithItsContainer(t *testing.T) {
	name := HopName("Bridge for desk_1!")
	if !regexp.MustCompile(`^decurix-hop-bridge-for-desk-1-[0-9a-f]{10}$`).MatchString(name) {
		t.Fatalf("name = %q", name)
	}
	if HopName("x") == HopName("x") {
		t.Fatal("two hops got the same name")
	}
	if !strings.HasPrefix(HopName("   "), "decurix-hop-run-") || len(HopName(strings.Repeat("a", 90))) > 60 {
		t.Fatal("an empty or long purpose")
	}

	args := hopArgs("decurix-hop-bridge-1", "bridge", true, "echo hi")
	joined := strings.Join(args, " ")
	for _, want := range []string{"run -i --rm --name decurix-hop-bridge-1", "--label decurix.hop=1", "--label decurix.hop.purpose=bridge",
		"--privileged --pid=host alpine nsenter -t 1 -m -u -n -i bash -c echo hi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
	if strings.Contains(strings.Join(hopArgs("n", "exec", false, "x"), " "), " -i --rm") {
		t.Error("a hop that takes no input keeps its stdin open")
	}

	cmd := HopCommand(nil, "/usr/bin/docker", "exec", false, "echo hi")
	if cmd.Cancel == nil || cmd.WaitDelay == 0 {
		t.Fatal("a hop has no way to be removed")
	}
	if cmd.Args[0] != "/usr/bin/docker" || cmd.Args[1] != "run" {
		t.Fatalf("args = %v", cmd.Args)
	}
}

// TestHopHelperProcess stands in for a docker client that never ends on its own (a bridge waiting on stdin).
func TestHopHelperProcess(t *testing.T) {
	if os.Getenv("DECURIX_HOP_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

// asHelper points a hop's command at the helper process above, keeping the hop's own Cancel.
func asHelper(cmd *exec.Cmd) {
	cmd.Path = os.Args[0]
	cmd.Args = []string{os.Args[0], "-test.run=^TestHopHelperProcess$"}
	cmd.Env = append(os.Environ(), "DECURIX_HOP_HELPER=1")
}

func TestCancellingAHopRemovesItsContainerNotOnlyTheClient(t *testing.T) {
	docker := &fakeDocker{}
	docker.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := HopCommand(ctx, "/usr/bin/docker", "bridge", true, "powershell.exe -File bridge.ps1")
	name := ""
	for i, a := range cmd.Args {
		if a == "--name" {
			name = cmd.Args[i+1]
		}
	}
	asHelper(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the client outlived its cancelled hop")
	}
	removed := docker.removed()
	if len(removed) != 1 || len(removed[0]) != 1 || removed[0][0] != name {
		t.Fatalf("removed = %v, want [%s]", removed, name)
	}
}

func TestEndingASessionByCallingCancelRemovesTheHop(t *testing.T) {
	// winhost ends a session that way (endHost): its context is never cancelled.
	docker := &fakeDocker{fail: true} // already gone: "No such container" is not an error worth a log line
	docker.install(t)
	cmd := HopCommand(context.Background(), "/usr/bin/docker", "bridge", true, "x")
	asHelper(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if len(docker.removed()) != 1 {
		t.Fatalf("removed = %v", docker.removed())
	}
}

func TestTheStartSweepRemovesOnlyHops(t *testing.T) {
	docker := &fakeDocker{ps: "decurix-hop-bridge-1a2b3c4d5e\ndecurix-hop-exec-ffeeddccbb\nmy-own-container\n\ndesktop_pc\n"}
	docker.install(t)
	if n := SweepHops(context.Background(), "/usr/bin/docker"); n != 2 {
		t.Fatalf("swept %d", n)
	}
	removed := docker.removed()
	if len(removed) != 1 || strings.Join(removed[0], " ") != "decurix-hop-bridge-1a2b3c4d5e decurix-hop-exec-ffeeddccbb" {
		t.Fatalf("removed = %v", removed)
	}
	ps := docker.calls[0]
	if strings.Join(ps[1:], " ") != "ps -a --filter label=decurix.hop=1 --format {{.Names}}" {
		t.Errorf("listed with %v", ps)
	}
	// Nothing to sweep: nothing removed.
	quiet := &fakeDocker{ps: "\n"}
	quiet.install(t)
	if SweepHops(context.Background(), "/usr/bin/docker") != 0 || len(quiet.removed()) != 0 {
		t.Error("an empty sweep removed something")
	}
	if SweepHops(context.Background(), "") != 0 {
		t.Error("swept with no docker")
	}
}

func TestTheStartSweepRemovesUnnamedHopsFromBeforeAndNothingElse(t *testing.T) {
	docker := &fakeDocker{ps: strings.Join([]string{
		"aaa\t\"nsenter -t 1 -m -u -n -i bash -c 'PATH=... powershell.exe -File bridge.ps1'\"\t",
		"bbb\t\"nsenter -t 1 -m -u -n -i bash -c 'echo named'\"\t1", // a named hop: the other sweep's
		"ccc\t\"sh -c 'cp -a /opt/tools/. /target/'\"\t",            // the tools volume's own init
		"ddd\t\"nsenter -t 2 -m bash\"\t",                           // not the hop's command
		"",
	}, "\n")}
	docker.install(t)
	if n := SweepUnnamedHops(context.Background(), "/usr/bin/docker"); n != 1 {
		t.Fatalf("swept %d", n)
	}
	if removed := docker.removed(); len(removed) != 1 || strings.Join(removed[0], " ") != "aaa" {
		t.Fatalf("removed = %v", removed)
	}
	if args := strings.Join(docker.calls[0][1:], " "); !strings.Contains(args, "--filter ancestor=alpine") {
		t.Errorf("listed with %s", args)
	}
}
