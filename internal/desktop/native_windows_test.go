//go:build windows

package desktop

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// realHelper runs winhost's real helper script, as the control plane does.
func realHelper(t *testing.T) winhost.Starter {
	t.Helper()

	ps := PowershellExe()
	if ps == "" {
		t.Skip("Windows PowerShell was not found")
	}

	script := filepath.Join(t.TempDir(), "bridge.ps1")
	if err := os.WriteFile(script, winhost.Script, 0o644); err != nil {
		t.Fatal(err)
	}

	return func(ctx context.Context, desktopName string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-DesktopName", desktopName)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		return cmd, nil
	}
}

// A real private desktop on this computer, with a program standing in for
// the agent: the worker starts, has a screen that can be pictured although
// nothing has a window on it, and ends - program and all - when it is
// stopped. This is the part no fake can show: that Windows does what
// native.go assumes.
func TestANativeWorkerRunsOnARealPrivateDesktop(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real helper process")
	}

	m := NewNativeManager(NativeRuntime{
		Root: t.TempDir(),
		// The "agent": a console program that just stays alive. ping is on
		// every Windows; its one argument is native_worker.py's path, which
		// it rejects as a host name only after its own timeout.
		Python:          filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe"),
		AgentDir:        "-n",
		Soul:            "unused",
		ControlPlaneURL: "http://127.0.0.1:1",
		Helper:          realHelper(t),
	})
	t.Cleanup(m.StopNativeWorkers)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A stand-in for the agent's arguments: "ping -n\native_worker.py" would
	// exit at once, so the worker is given a real long-running command line
	// by starting the session the way start() does, with ping's own.
	meta := nativeMeta{ID: "probe", Name: "Probe", Harness: HarnessTheBot, Brain: BrainClaude, CreatedAt: time.Now().Format(time.RFC3339)}
	if err := m.native.writeMeta(meta); err != nil {
		t.Fatal(err)
	}

	session, err := winhost.OpenDesktop(ctx, m.native.rt.Helper, "w_probe", m.native.env(meta, 1, 2, 3), m.native.rt.Python, "-n 120 127.0.0.1", m.native.rt.Root)
	if err != nil {
		t.Fatalf("OpenDesktop: %v", err)
	}
	m.native.mu.Lock()
	m.native.running["probe"] = &nativeRun{session: session, agentPort: 1, soulPort: 2, startedAt: time.Now()}
	m.native.mu.Unlock()

	if why := session.Running(); why != "" {
		t.Fatalf("the worker's program is not running: %s", why)
	}

	got, err := m.Get(ctx, "probe")
	if err != nil || got.Status != "running" || got.StartedAt == "" {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	// The screen of a worker with nothing open is still a picture: the
	// whole private desktop, the size of this computer's screen.
	shot, err := m.Screenshot(ctx, "probe", 480)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(shot))
	if err != nil || cfg.Width != 480 || cfg.Height < 100 {
		t.Fatalf("the thumbnail is %dx%d, %v", cfg.Width, cfg.Height, err)
	}

	if s, err := m.NativeSession("probe"); err != nil || s != session {
		t.Fatalf("NativeSession = %v, %v", s, err)
	}

	if err := m.Stop(ctx, "probe"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the helper is still running after Stop")
	}

	if got, _ := m.Get(ctx, "probe"); got.Status != "stopped" || got.Crash != "" {
		t.Errorf("after Stop the worker is %q (crash %q)", got.Status, got.Crash)
	}
	if _, err := m.NativeSession("probe"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("NativeSession of a stopped worker = %v, want ErrNotRunning", err)
	}
}
