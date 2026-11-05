//go:build linux

package isolate

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A confined process (no network, a view of its own files) cannot reach a
// Unix socket outside its policy, nor read a file there, and still serves
// on its own socket in its writable directory, which this side reaches.
// The outside socket is one the test makes, never a real daemon's.
func TestAConfinedProcessCannotReachASocketOutsideItsPolicy(t *testing.T) {
	if os.Getenv("BRAINIAC_VIEW_CHILD") == "1" {
		viewChild()
	}
	if ok, err := CanConfine(); !ok {
		t.Skipf("this system refuses the namespaces: %v", err)
	}
	outside, own := t.TempDir(), t.TempDir()
	outsideSock := filepath.Join(outside, "daemon.sock")
	l, err := net.Listen("unix", outsideSock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	mustWrite(t, filepath.Join(outside, "secret"), "secret")
	// A file in the host's shared memory, which a view's private /dev/shm hides.
	shm := ""
	if f, err := os.CreateTemp("/dev/shm", "brainiac-view-test-"); err == nil {
		shm = f.Name()
		f.Close()
		defer os.Remove(shm)
	}
	self, _ := os.Executable()
	cmd := exec.Command(self, "-test.run=TestAConfinedProcessCannotReachASocketOutsideItsPolicy")
	cmd.Env = Env("BRAINIAC_VIEW_CHILD=1", "CHILD_OUTSIDE_SOCK="+outsideSock, "CHILD_SECRET="+filepath.Join(outside, "secret"),
		"CHILD_OWN="+own, "CHILD_PARENT="+strconv.Itoa(os.Getpid()), "CHILD_HOST_SHM="+shm)
	NoNetwork(cmd)
	DieWithParent(cmd)
	if err := Wrap(cmd, ForProgram(self, nil, []string{own})); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := StartLocked(cmd); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ownSock := filepath.Join(own, "s.sock")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if c, err := net.Dial("unix", ownSock); err == nil {
			fmt.Fprintln(c, "hello")
			c.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the child ended before serving: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("its own socket never answered\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func viewChild() {
	fail := func(code int, what string, err error) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
		os.Exit(code)
	}
	if c, err := net.Dial("unix", os.Getenv("CHILD_OUTSIDE_SOCK")); err == nil {
		c.Close()
		fail(41, "reached a socket outside its policy", nil)
	}
	if _, err := os.ReadFile(os.Getenv("CHILD_SECRET")); err == nil {
		fail(42, "read a file outside its policy", nil)
	}
	if _, err := os.ReadFile("/proc/" + os.Getenv("CHILD_PARENT") + "/environ"); err == nil {
		fail(43, "read its parent's environment", nil)
	}
	if _, err := os.Stat("/run"); err == nil {
		if entries, _ := os.ReadDir("/run"); len(entries) > 0 {
			fail(44, "sees the host's /run", nil)
		}
	}
	if _, err := os.ReadFile("/proc/cpuinfo"); err != nil {
		fail(45, "could not read the machine's facts", err)
	}
	if p := os.Getenv("CHILD_HOST_SHM"); p != "" {
		if _, err := os.Stat(p); err == nil {
			fail(48, "sees the host's /dev/shm", nil)
		}
	}
	if err := os.WriteFile("/dev/null", []byte("x"), 0); err != nil {
		fail(49, "could not write /dev/null", err)
	}
	if f, err := os.Open("/dev/urandom"); err != nil {
		fail(50, "could not open /dev/urandom", err)
	} else {
		b := make([]byte, 16)
		if _, err := f.Read(b); err != nil {
			fail(50, "could not read /dev/urandom", err)
		}
		f.Close()
	}
	if err := os.WriteFile("/dev/shm/x", []byte("x"), 0o600); err != nil {
		fail(51, "has no /dev/shm of its own", err)
	}
	if entries, _ := os.ReadDir("/dev"); len(entries) > len(viewDevices)+8 {
		fail(52, fmt.Sprintf("sees the host's /dev (%d entries)", len(entries)), nil)
	}
	l, err := net.Listen("unix", filepath.Join(os.Getenv("CHILD_OWN"), "s.sock"))
	if err != nil {
		fail(46, "could not listen on its own socket", err)
	}
	l.(*net.UnixListener).SetDeadline(time.Now().Add(15 * time.Second))
	c, err := l.Accept()
	if err != nil {
		fail(47, "no one reached its own socket", err)
	}
	c.Close()
	os.Exit(0)
}
