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
	"strings"
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
		// The "agent": a console program that just stays alive and says
		// nothing - PowerShell asleep. ping would do, but it writes a line a
		// second into the helper's own output, in among its answers.
		Python:          PowershellExe(),
		AgentDir:        "-NoProfile",
		Soul:            "unused",
		ControlPlaneURL: "http://127.0.0.1:1",
		Helper:          realHelper(t),
	})
	t.Cleanup(m.StopNativeWorkers)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A stand-in for the agent's arguments: "powershell -NoProfile\native_worker.py"
	// would exit at once, so the worker is given a real long-running command
	// line by starting the session the way start() does, with its own.
	meta := nativeMeta{ID: "probe", Name: "Probe", Harness: HarnessTheBot, Brain: BrainClaude, CreatedAt: time.Now().Format(time.RFC3339),
		Apps: []NativeApp{{ID: "chatgpt", Name: "ChatGPT", Kind: NativeAppWeb, Target: "https://chatgpt.com"}}}
	if err := m.native.writeMeta(meta); err != nil {
		t.Fatal(err)
	}

	session, err := winhost.OpenDesktop(ctx, m.native.rt.Helper, "w_probe", m.native.env(meta, 1, 2, 3), m.native.rt.Python, "-NoProfile -NonInteractive -Command Start-Sleep 120", m.native.rt.Root)
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

	// The desktop has its icons: a double-click on the first one (the cell
	// in the wallpaper's top-left corner, at any display scaling) is said to
	// the control plane, which is what knows what the shortcut opens.
	opened := make(chan string, 4)
	session.OnEvent(func(event string) { opened <- event })
	if err := session.SetShortcuts("Probe", shortcuts(meta)); err != nil {
		t.Fatalf("SetShortcuts: %v", err)
	}
	if session.OverWindow(60, 60) {
		t.Fatal("the wallpaper's corner is reported as a window")
	}
	for i := 0; i < 2; i++ {
		if err := firstError(session.Mouse("down", 60, 60, false, ""), session.Mouse("up", 60, 60, false, "")); err != nil {
			t.Fatalf("pressing an icon: %v", err)
		}
	}
	select {
	case event := <-opened:
		if event != "open "+meta.Apps[0].ID {
			t.Errorf("a double-click on the first icon said %q, want open %s", event, meta.Apps[0].ID)
		}
	case <-time.After(5 * time.Second):
		t.Error("a double-click on an icon said nothing")
	}

	// A shortcut opens a program on this same desktop, where the helper
	// then counts one more window (the stand-in agent is a console program,
	// so its console may be there already - a real agent runs windowless).
	// The session goes on speaking of the agent it was started for.
	before, err := session.Do("count")
	if err != nil || !strings.HasPrefix(before, "COUNT ") {
		t.Fatalf("count = %q, %v", before, err)
	}
	charmap := NativeApp{ID: "charmap", Name: "Character Map", Kind: NativeAppProgram, Target: filepath.Join(os.Getenv("SystemRoot"), "System32", "charmap.exe")}
	if err := m.OpenNativeProgram("probe", charmap); err != nil {
		t.Fatalf("OpenNativeProgram: %v", err)
	}
	after := before
	for i := 0; i < 100 && after == before; i++ {
		time.Sleep(200 * time.Millisecond)
		after, _ = session.Do("count")
	}
	if after == before || m.NativeScreenEmpty("probe") {
		t.Fatalf("Character Map was started but no window appeared on the worker's desktop (%s, then %s)", before, after)
	}
	if why := session.Running(); why != "" {
		t.Fatalf("after opening an app the worker's own program is reported as: %s", why)
	}
	if err := m.OpenNativeProgram("probe", NativeApp{Name: "Nothing", Kind: NativeAppProgram, Target: filepath.Join(os.Getenv("SystemRoot"), "no-such-program.exe")}); err == nil {
		t.Error("opening a program that is not on this computer reported success")
	}

	// The taskbar: the window has a button, which brings it to the front and
	// closes it.
	var charmapWindow int64
	for _, w := range m.NativeWindows("probe") {
		if w.Title == "Character Map" {
			charmapWindow = w.ID
		}
	}
	if charmapWindow == 0 {
		t.Fatalf("the taskbar lists %+v, with no Character Map", m.NativeWindows("probe"))
	}
	if err := m.NativeWindowDo("probe", "raise", charmapWindow); err != nil {
		t.Errorf("raise: %v", err)
	}
	if front := m.NativeWindows("probe"); len(front) == 0 || front[0].ID != charmapWindow || !front[0].Active {
		t.Errorf("after raising Character Map the taskbar's front window is %+v", front)
	}
	if err := m.NativeWindowDo("probe", "close", charmapWindow); err != nil {
		t.Errorf("close: %v", err)
	}
	gone := false
	for i := 0; i < 50 && !gone; i++ {
		time.Sleep(200 * time.Millisecond)
		gone = true
		for _, w := range m.NativeWindows("probe") {
			gone = gone && w.ID != charmapWindow
		}
	}
	if !gone {
		t.Error("Character Map is still on the taskbar after its button closed it")
	}
	if err := m.NativeWindowDo("probe", "raise", charmapWindow); err == nil {
		t.Error("raising a window that has closed reported success")
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

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
