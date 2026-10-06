//go:build windows

package desktop

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// realHelper runs winhost's real helper, as the control plane does: hosted by
// Windows PowerShell, or (asProgram) as the program that script compiles.
func realHelper(t *testing.T, asProgram bool) winhost.Starter {
	t.Helper()

	ps := PowershellExe()
	if ps == "" {
		t.Skip("Windows PowerShell was not found")
	}

	script := filepath.Join(t.TempDir(), "bridge.ps1")
	if err := os.WriteFile(script, winhost.Script, 0o644); err != nil {
		t.Fatal(err)
	}

	hide := func(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

	program := ""
	if asProgram {
		t.Setenv("DECURIX_SCREEN_HELPER", "")
		if program = winhost.ScreenProgram(ps, script, hide); program == "" {
			t.Skip("the helper could not be built as a program on this computer")
		}
	}

	return func(ctx context.Context, desktopName string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-DesktopName", desktopName)
		if program != "" {
			cmd = exec.CommandContext(ctx, program, "-DesktopName", desktopName, "-Script", script)
		}
		hide(cmd)
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

	// Both ways the helper runs must do the same: the program a worker's
	// desktop normally has, and the PowerShell that hosts the same code where
	// the program cannot be built or run.
	t.Run("as a program", func(t *testing.T) { aWorkerOnARealDesktop(t, true) })
	t.Run("hosted by PowerShell", func(t *testing.T) { aWorkerOnARealDesktop(t, false) })
}

func aWorkerOnARealDesktop(t *testing.T, asProgram bool) {
	m := NewNativeManager(NativeRuntime{
		Root: t.TempDir(),
		// The "agent": a console program that just stays alive and says
		// nothing - PowerShell asleep. ping would do, but it writes a line a
		// second into the helper's own output, in among its answers.
		Python:          PowershellExe(),
		AgentDir:        "-NoProfile",
		Soul:            "unused",
		ControlPlaneURL: "http://127.0.0.1:1",
		Helper:          realHelper(t, asProgram),
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

	// A console program - the worker's Terminal - has a window of its own on
	// the worker's desktop and leaves the helper's own input and output
	// alone. Found live: it was given them instead, had no window, took the
	// commands meant for the helper and wrote its prompt in among the answers.
	terminal := NativeApp{ID: "terminal", Name: "Terminal", Kind: NativeAppProgram, Target: PowershellExe(), Args: `-NoLogo -NoProfile -NoExit -Command "$Host.UI.RawUI.WindowTitle = 'Probe terminal'"`}
	if err := m.OpenNativeProgram("probe", terminal); err != nil {
		t.Fatalf("opening a terminal: %v", err)
	}
	var terminalWindow int64
	for i := 0; i < 100 && terminalWindow == 0; i++ {
		time.Sleep(200 * time.Millisecond)
		for _, w := range m.NativeWindows("probe") {
			if strings.Contains(w.Title, "Probe terminal") {
				terminalWindow = w.ID
			}
		}
	}
	if terminalWindow == 0 {
		t.Fatalf("a terminal was opened and has no window on the worker's desktop: %+v", m.NativeWindows("probe"))
	}
	for i := 0; i < 20; i++ {
		if said, err := session.Do("count"); err != nil || !strings.HasPrefix(said, "COUNT ") {
			t.Fatalf("with a terminal open the helper answered count with %q, %v", said, err)
		}
	}

	// What is typed on the worker's screen reaches it: "exit" ends it.
	if err := firstError(m.NativeWindowDo("probe", "raise", terminalWindow), session.Type("exit"), session.Key(13, "")); err != nil {
		t.Fatalf("typing into the terminal: %v", err)
	}
	closed := false
	for i := 0; i < 50 && !closed; i++ {
		time.Sleep(200 * time.Millisecond)
		closed = true
		for _, w := range m.NativeWindows("probe") {
			closed = closed && w.ID != terminalWindow
		}
	}
	if !closed {
		t.Error(`"exit" typed into the terminal did not end it`)
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

// A worker's Terminal is the worker's, not the person's: a shell in a sandbox
// that starts in the worker's work folder and can use it, and that can
// neither read nor write the person's own files, nor reach anything listening
// on this computer - a control plane among them. Asked of the terminal itself,
// by typing into it on a real private desktop, both ways the helper runs.
// Found live: the Terminal was simply the person's PowerShell, on a screen
// that can be opened from a phone.
func TestAWorkersTerminalIsConfinedToTheWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real helper process")
	}

	t.Run("as a program", func(t *testing.T) { aConfinedTerminal(t, true) })
	t.Run("hosted by PowerShell", func(t *testing.T) { aConfinedTerminal(t, false) })
}

func aConfinedTerminal(t *testing.T, asProgram bool) {
	// Under the person's own profile, as a worker's folder is; and by its
	// full name, which t.TempDir() is not on every computer.
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		t.Skip("no LOCALAPPDATA")
	}
	root, err := os.MkdirTemp(local, "decurix-terminal-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// Something listening on this computer, as a control plane does.
	var reached atomic.Int32
	listening := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer listening.Close()

	m := NewNativeManager(NativeRuntime{Root: root, Python: PowershellExe(), AgentDir: "-NoProfile", Soul: "unused", ControlPlaneURL: listening.URL, Helper: realHelper(t, asProgram)})
	t.Cleanup(m.StopNativeWorkers)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	meta := nativeMeta{ID: "probe", Name: "Probe", Harness: HarnessTheBot, Brain: BrainClaude, CreatedAt: time.Now().Format(time.RFC3339)}
	if err := m.native.writeMeta(meta); err != nil {
		t.Fatal(err)
	}
	session, err := winhost.OpenDesktop(ctx, m.native.rt.Helper, "w_term", m.native.env(meta, 1, 2, 3), m.native.rt.Python, "-NoProfile -NonInteractive -Command Start-Sleep 120", m.native.rt.Root)
	if err != nil {
		t.Fatalf("OpenDesktop: %v", err)
	}
	m.native.mu.Lock()
	m.native.running["probe"] = &nativeRun{session: session, agentPort: 1, soulPort: 2, startedAt: time.Now()}
	m.native.mu.Unlock()

	var terminal NativeApp
	for _, app := range m.native.withFilesApp("probe", nil) {
		if app.ID == NativeTerminalAppID {
			terminal = app
		}
	}
	if !terminal.Confined {
		t.Fatalf("the worker's Terminal shortcut is not confined: %+v", terminal)
	}
	if err := m.OpenNativeProgram("probe", terminal); err != nil {
		t.Fatalf("opening the terminal: %v", err)
	}

	var window int64
	for i := 0; i < 100 && window == 0; i++ {
		time.Sleep(200 * time.Millisecond)
		for _, w := range m.NativeWindows("probe") {
			if w.Title == "Terminal" {
				window = w.ID
			}
		}
	}
	if window == 0 {
		t.Fatalf("the terminal has no window on the worker's desktop: %+v", m.NativeWindows("probe"))
	}

	work := filepath.Join(m.native.home("probe"), "work")
	profile := os.Getenv("USERPROFILE")
	secret := filepath.Join(profile, "decurix-terminal-test.txt")
	t.Cleanup(func() { _ = os.Remove(secret) })

	// Doubled letters among them ("bool", ">>"): typed into a console, each
	// used to come out three times.
	lines := []string{
		`"where=" + (Get-Location).Path > out.txt`,
		`"read=" + [bool](Get-ChildItem -LiteralPath '` + profile + `' -ErrorAction SilentlyContinue) >> out.txt`,
		`try { [IO.File]::WriteAllText('` + secret + `', 'x'); "write=True" >> out.txt } catch { "write=False" >> out.txt }`,
		`"above=" + [bool](Get-Content -LiteralPath '` + filepath.Join(filepath.Dir(filepath.Dir(work)), "worker.json") + `' -ErrorAction SilentlyContinue) >> out.txt`,
		`cmd /c "curl.exe -s -m 4 -o NUL ` + listening.URL + `/"; "local=" + $LASTEXITCODE >> out.txt`,
		`mkdir sub | Out-Null; cd sub; cmd /c "echo made> made.txt"; "native=" + (Get-Content made.txt) + "@" + (Get-Location).Path >> ..\out.txt; cd ..`,
		`$listing = ls | Out-String; "listingPath=" + $listing.Contains('` + work + `') >> out.txt; "listingFile=" + $listing.Contains('out.txt') >> out.txt`,
		`"told=" + $env:CONTROL_PLANE_URL + $env:DESKTOP_ID + "|" + $env:USERPROFILE >> out.txt`,
		`"done" >> out.txt`,
	}
	if err := m.NativeWindowDo("probe", "raise", window); err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if err := firstError(session.Type(line), session.Key(13, "")); err != nil {
			t.Fatalf("typing into the terminal: %v", err)
		}
		time.Sleep(400 * time.Millisecond)
	}

	said := ""
	for i := 0; i < 120 && !strings.Contains(said, "done"); i++ {
		time.Sleep(500 * time.Millisecond)
		raw, _ := os.ReadFile(filepath.Join(work, "out.txt"))
		said = strings.ReplaceAll(strings.TrimPrefix(string(raw), "\xff\xfe"), "\x00", "")
	}
	answers := map[string]string{}
	for _, line := range strings.Split(said, "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			answers[name] = value
		}
	}
	t.Logf("the terminal said:\n%s", said)

	for name, want := range map[string]string{
		"where":       `Work:\`,
		"read":        "False",
		"write":       "False",
		"above":       "False",
		"native":      `made@Work:\sub`,
		"listingPath": "False",
		"listingFile": "True",
	} {
		if answers[name] != want {
			t.Errorf("%s = %q, want %q", name, answers[name], want)
		}
	}
	if answers["local"] == "" || answers["local"] == "0" || reached.Load() != 0 {
		t.Errorf("the terminal reached a server on this computer (curl exit %q, %d requests)", answers["local"], reached.Load())
	}
	if told := answers["told"]; strings.Contains(told, "127.0.0.1") || strings.Contains(told, "probe") || strings.Contains(strings.ToLower(told), strings.ToLower(profile)+"|") || strings.HasSuffix(strings.ToLower(told), "|"+strings.ToLower(profile)) {
		t.Errorf("the terminal was told the worker's own settings or the person's profile: %q", told)
	}
	if _, err := os.Stat(secret); err == nil {
		t.Error("the terminal wrote a file into the person's profile")
	}
}
