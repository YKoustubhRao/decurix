package desktop

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nativeForTest is a native Manager whose workers live in a temp folder. It
// has no helper: these tests are about what a worker is on disk, which needs
// no private desktop.
func nativeForTest(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("DECURIX_CONTROL_PLANE_URL", "")

	return NewNativeManager(NativeRuntime{
		Root:            t.TempDir(),
		Python:          filepath.Join(testRuntime, "python", "pythonw.exe"),
		AgentDir:        filepath.Join(testRuntime, "agent"),
		Soul:            filepath.Join(testRuntime, "soul.exe"),
		UIHelper:        filepath.Join(testRuntime, "decurix-ui.exe"),
		ControlPlaneURL: "http://127.0.0.1:8080",
	})
}

// testRuntime is where the shipped runtime is said to be in these tests. Its
// paths are joined with this system's separator: the tests run on Linux too,
// where a C:\ path is one file name.
var testRuntime = filepath.Join(os.TempDir(), "decurix-runtime")

func writeWorker(t *testing.T, m *Manager, meta nativeMeta) {
	t.Helper()
	if err := m.native.writeMeta(meta); err != nil {
		t.Fatal(err)
	}
}

func TestANativeWorkerIsListedFromItsRecord(t *testing.T) {
	m := nativeForTest(t)
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada", Profile: "basic", Harness: HarnessTheBot, Brain: BrainClaude, Model: "claude-opus-5-5", CreatedAt: "2026-10-01T10:00:00Z", Specialization: "invoices"})
	writeWorker(t, m, nativeMeta{ID: "bo", Name: "Bo", Harness: HarnessTheBot, Brain: BrainOllama, CreatedAt: "2026-10-01T09:00:00Z", Stopped: true})
	// A folder that is not a worker is not listed as one.
	if err := os.MkdirAll(filepath.Join(m.native.rt.Root, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}

	list, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "bo" || list[1].ID != "ada" {
		t.Fatalf("listed %+v, want bo then ada (oldest first)", list)
	}

	ada := list[1]
	if ada.Name != "Ada" || ada.Brain != BrainClaude || ada.Model != "claude-opus-5-5" || ada.Specialization != "invoices" {
		t.Errorf("ada = %+v", ada)
	}
	if ada.Status != "stopped" || ada.URL != "/desktop/ada/" {
		t.Errorf("a worker with no desktop open is %q at %q", ada.Status, ada.URL)
	}

	got, err := m.Get(context.Background(), "Ada")
	if err != nil || got.ID != "ada" {
		t.Errorf("Get(Ada) = %+v, %v", got, err)
	}
	if _, err := m.Get(context.Background(), "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a worker that is not there = %v, want ErrNotFound", err)
	}
	if !m.Native() || m.UsesLocalModel(context.Background()) != true {
		t.Errorf("Native() = %v, UsesLocalModel() = %v (bo is on the Ollama brain)", m.Native(), m.UsesLocalModel(context.Background()))
	}
}

func TestANativeWorkersEnvironmentIsItsOwnFoldersAndPorts(t *testing.T) {
	m := nativeForTest(t)
	meta := nativeMeta{ID: "ada", Name: "Ada", Harness: HarnessTheBot, Brain: BrainAzure, BrainModel: "gpt-5.4", BrainVision: true}

	env := m.native.env(meta, 51001, 51002, 51003)
	home := filepath.Join(m.native.rt.Root, "ada", "home")

	for name, want := range map[string]string{
		"DESKTOP_ID":         "ada",
		"CONTROL_PLANE_URL":  "http://127.0.0.1:8080",
		"AGENT_HARNESS":      HarnessTheBot,
		"DECURIX_DIR":        filepath.Join(home, ".decurix"),
		"DECURIX_WORK_DIR":   filepath.Join(home, "work"),
		"DECURIX_AGENT_BIND": "127.0.0.1",
		"DECURIX_AGENT_PORT": "51001",
		"SOUL_HOST":          "127.0.0.1",
		"SOUL_PORT":          "51002",
		"THEBOT_URL":         "http://127.0.0.1:51002",
		"DECURIX_SOUL":       filepath.Join(testRuntime, "soul.exe"),
		"DECURIX_PYTHON":     filepath.Join(testRuntime, "python", "python.exe"),
		"DECURIX_UI_HELPER":  filepath.Join(testRuntime, "decurix-ui.exe"),
		"DECURIX_CDP_PORT":   "51003",
		// A hosted brain is reached through the control plane on loopback.
		"OPENAI_BASE_URL":      "http://127.0.0.1:8080/api/llm/azure",
		"DECURIX_BRAIN_VISION": "1",
	} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// What a Linux desktop is given and Windows must not be: a PATH of Linux
	// folders would hide every Windows program from the agent.
	for _, name := range []string{"PATH", "VNC_PW", "GOROOT", "HF_HOME"} {
		for _, entry := range env {
			if strings.HasPrefix(entry, name+"=") {
				t.Errorf("a native worker was given %s", entry)
			}
		}
	}
}

func TestFilesGoIntoAndComeOutOfANativeWorkersHome(t *testing.T) {
	m := nativeForTest(t)
	ctx := context.Background()
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada"})

	files := []File{{Name: "invoice.pdf", Data: []byte("%PDF")}, {Name: `C:\mail\notes.txt`, Data: []byte("hello")}}
	if err := m.CopyFiles(ctx, "ada", "/home/headless/work/inbox/7", files); err != nil {
		t.Fatal(err)
	}

	inbox := filepath.Join(m.native.rt.Root, "ada", "home", "work", "inbox", "7")
	if data, err := os.ReadFile(filepath.Join(inbox, "notes.txt")); err != nil || string(data) != "hello" {
		t.Errorf("notes.txt = %q, %v (a sender's folder is not part of the name)", data, err)
	}

	// The agent is told where a file is by its real place on this computer.
	if got := m.AgentPath("ada", "/home/headless/work/inbox/7/notes.txt"); got != filepath.Join(inbox, "notes.txt") {
		t.Errorf("AgentPath = %q, want %q", got, filepath.Join(inbox, "notes.txt"))
	}
	if got := m.AgentPath("ada", "/etc/passwd"); got != "/etc/passwd" {
		t.Errorf("a path outside the home became %q", got)
	}

	for _, outside := range []string{"/home/headless", "/etc/passwd", "/home/headless/../../windows"} {
		if err := m.CopyFiles(ctx, "ada", outside, files); err == nil {
			t.Errorf("files were copied to %s, outside the worker's home", outside)
		}
	}
	if err := m.CopyFiles(ctx, "nobody", "/home/headless/work", files); !errors.Is(err, ErrNotFound) {
		t.Errorf("CopyFiles into a worker that is not there = %v, want ErrNotFound", err)
	}

	// Out again, as the tar a backup reads: named from the folder down.
	rc, err := m.CopyOut(ctx, "ada", "/home/headless/work/inbox")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		got[header.Name] = string(data)
	}
	if got["inbox/7/invoice.pdf"] != "%PDF" || got["inbox/7/notes.txt"] != "hello" {
		t.Errorf("CopyOut gave %v", got)
	}
	if _, isDir := got["inbox/"]; !isDir {
		t.Errorf("CopyOut left out the folder itself: %v", got)
	}

	if _, err := m.CopyOut(ctx, "ada", "/home/headless/.decurix/skills"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CopyOut of a folder the worker does not have = %v, want ErrNotFound", err)
	}
}

func TestWorkerMemoryIsWrittenWhereANativeAgentReadsIt(t *testing.T) {
	m := nativeForTest(t)
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada"})

	if err := m.PersistMemoryFile(context.Background(), "ada", "../../escape.txt", "  prefers PDF invoices \n"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(m.native.rt.Root, "ada", "home", ".decurix", "memory", "escape.txt"))
	if err != nil || string(data) != "prefers PDF invoices\n" {
		t.Errorf("memory file = %q, %v (a name is only ever a name, inside the memory folder)", data, err)
	}
}

func TestStoppingAndDeletingANativeWorker(t *testing.T) {
	m := nativeForTest(t)
	ctx := context.Background()
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada"})

	// Stopped by a person: remembered, so the next start of the control
	// plane leaves it stopped.
	if err := m.Stop(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	if meta, _ := m.native.readMeta("ada"); !meta.Stopped {
		t.Error("a worker a person stopped was not recorded as stopped")
	}

	if err := m.SendRequest(ctx, "ada", "hello", ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a request to a stopped worker = %v, want ErrNotRunning (the caller starts it and tries again)", err)
	}
	if _, _, err := m.ProxyTarget(ctx, "ada"); !errors.Is(err, ErrNoVNC) {
		t.Errorf("ProxyTarget = %v, want ErrNoVNC", err)
	}

	if err := m.Delete(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.native.rt.Root, "ada")); !os.IsNotExist(err) {
		t.Errorf("the worker's folder is still there after Delete: %v", err)
	}
	if err := m.Delete(ctx, "ada"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it again = %v, want ErrNotFound", err)
	}
}

func TestANativeManagerNeverReachesForDocker(t *testing.T) {
	m := nativeForTest(t)
	ctx := context.Background()

	// Each of these would dereference the nil Docker client.
	if missing := m.MissingImages(ctx); missing != nil {
		t.Errorf("MissingImages = %v", missing)
	}
	if err := m.EnsureImages(ctx); err != nil {
		t.Errorf("EnsureImages: %v", err)
	}
	if err := m.EnsureNetwork(ctx); err != nil {
		t.Errorf("EnsureNetwork: %v", err)
	}
	if err := m.EnsureToolsVolume(ctx); err != nil {
		t.Errorf("EnsureToolsVolume: %v", err)
	}
	if ips := m.GatewayIPs(ctx); ips != nil {
		t.Errorf("GatewayIPs = %v", ips)
	}
	if stale := m.StaleControlPlane(ctx); stale != nil {
		t.Errorf("StaleControlPlane = %v", stale)
	}
	// Apps become shortcuts on a worker's screen; there is no worker "ada" here.
	if err := m.MigrateHostApps(ctx, "ada", []string{"chrome"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("MigrateHostApps for a worker that is not there = %v, want ErrNotFound", err)
	}
	if _, _, err := m.StartClaudeLogin(ctx); err == nil {
		t.Error("the Claude login, which needs a container, started")
	}
}

// The apps picked for a native worker are shortcuts, not copies: a service's
// web version, or a program the scan confirmed can run on a private desktop.
// Credentials to copy and apps that cannot be shown mean nothing there.
func TestTheAppsPickedForANativeWorkerBecomeShortcuts(t *testing.T) {
	detected := []HostAppItem{
		{ID: "chatgpt", Name: "ChatGPT", Mode: ModeWeb},
		{ID: "win_google-chrome", Name: "Google Chrome", Mode: ModeHostGUI, HostCmd: `cmd.exe /c start explorer.exe shell:AppsFolder\Chrome`},
		{ID: "win_code", Name: "Code", Mode: ModeHostGUI, HostCmd: `cmd.exe /c start "" "C:\Programs\VS Code\Code.exe" --new-window`},
		{ID: "ssh", Name: "SSH keys", Mode: ModeData},
		{ID: "win_calculator", Name: "Calculator", Mode: ModeUnavailable, HostCmd: `cmd.exe /c start explorer.exe shell:AppsFolder\Calc!App`},
	}

	got := nativeApps([]string{"chatgpt", "WIN_GOOGLE-CHROME", "win_code", "ssh", "win_calculator", "nobody", "chatgpt"}, detected)

	want := []NativeApp{
		{ID: "chatgpt", Name: "ChatGPT", Kind: NativeAppWeb, Target: "https://chatgpt.com"},
		{ID: "win_google-chrome", Name: "Google Chrome", Kind: NativeAppProgram, Target: `shell:AppsFolder\Chrome`},
		{ID: "win_code", Name: "Code", Kind: NativeAppProgram, Target: `C:\Programs\VS Code\Code.exe`, Args: "--new-window"},
	}
	if len(got) != len(want) {
		t.Fatalf("shortcuts = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("shortcut %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A browser picked for a worker is the worker's browser: its agent is told
// which, so the pages it opens are in that browser and not in another one
// beside it. Found live: Chrome was copied to a worker and the agent used Edge.
func TestTheBrowserPickedForAWorkerIsTheOneItsAgentUses(t *testing.T) {
	for _, c := range []struct {
		app  NativeApp
		want string
	}{
		{NativeApp{Name: "Google Chrome", Kind: NativeAppProgram, Target: `shell:AppsFolder\Chrome`}, "chrome"},
		{NativeApp{Name: "Chrome", Kind: NativeAppProgram, Target: `C:\Program Files\Google\Chrome\Application\chrome.exe`}, "chrome"},
		{NativeApp{Name: "Brave", Kind: NativeAppProgram, Target: `shell:AppsFolder\Brave`}, "brave"},
		{NativeApp{Name: "Microsoft Edge", Kind: NativeAppProgram, Target: `shell:AppsFolder\MSEdge`}, "msedge"},
		// A page of a browser, a web shortcut and another program are not the browser.
		{NativeApp{Name: "YouTube", Kind: NativeAppProgram, Target: `C:\chrome.exe`, Args: "--app=https://youtube.com"}, ""},
		{NativeApp{Name: "Chrome", Kind: NativeAppWeb, Target: "https://chrome.google.com"}, ""},
		{NativeApp{Name: "Code", Kind: NativeAppProgram, Target: `C:\Code.exe`}, ""},
	} {
		if got := browserKind(c.app); got != c.want {
			t.Errorf("browserKind(%+v) = %q, want %q", c.app, got, c.want)
		}
	}

	m := nativeForTest(t)
	meta := nativeMeta{ID: "ada", Name: "Ada", Harness: HarnessTheBot, Brain: BrainClaude}
	writeWorker(t, m, meta)
	file := filepath.Join(m.native.home("ada"), ".decurix", browserChoiceFile)

	m.native.noteBrowser(meta)
	if _, err := os.Stat(file); err == nil {
		t.Fatal("a worker with no browser picked was told it has one")
	}

	err := m.native.addAndRedress("ada", []NativeApp{
		{ID: "win_code", Name: "Code", Kind: NativeAppProgram, Target: `C:\Code.exe`},
		{ID: "win_chrome", Name: "Google Chrome", Kind: NativeAppProgram, Target: `C:\Apps\chrome.exe`},
		{ID: "win_edge", Name: "Microsoft Edge", Kind: NativeAppProgram, Target: `shell:AppsFolder\MSEdge`},
	})
	if err != nil {
		t.Fatal(err)
	}

	var choice map[string]string
	data, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &choice) != nil {
		t.Fatalf("the agent was not told its browser: %v (%s)", err, data)
	}
	if choice["kind"] != "chrome" || choice["exe"] != `C:\Apps\chrome.exe` {
		t.Errorf("the agent was told %v, want the first browser picked: Chrome at its own path", choice)
	}
}

// What is typed into the Start panel's search is opened as the address it is,
// or looked up.
func TestTheStartPanelsSearchOpensAnAddressOrLooksTheWordsUp(t *testing.T) {
	for words, want := range map[string]string{
		"https://example.com/a?b=c": "https://example.com/a?b=c",
		"erp.example.com":           "erp.example.com",
		"localhost:8000/app":        "localhost:8000/app",
		"gold price today":          "https://www.google.com/search?q=gold+price+today",
		"what is 7+8?":              "https://www.google.com/search?q=what+is+7%2B8%3F",
		"notes":                     "https://www.google.com/search?q=notes",
	} {
		if got := webTarget(words); got != want {
			t.Errorf("webTarget(%q) = %q, want %q", words, got, want)
		}
	}
}

func TestANativeWorkerKeepsItsShortcutsWhenMoreAreAdded(t *testing.T) {
	m := nativeForTest(t)
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada", Harness: HarnessTheBot, Brain: BrainClaude,
		Apps: []NativeApp{{ID: "chatgpt", Name: "ChatGPT", Kind: NativeAppWeb, Target: "https://chatgpt.com"}}})

	err := m.native.addNativeApps("ada", []NativeApp{
		{ID: "chatgpt", Name: "ChatGPT", Kind: NativeAppWeb, Target: "https://chatgpt.com/new"},
		{ID: "win_code", Name: "Code", Kind: NativeAppProgram, Target: `C:\Code.exe`},
	})
	if err != nil {
		t.Fatal(err)
	}

	apps, err := m.NativeApps("Ada")
	if err != nil || len(apps) != 2 || apps[0].Target != "https://chatgpt.com/new" || apps[1].ID != "win_code" {
		t.Fatalf("NativeApps = %+v, %v: want ChatGPT replaced in place and Code added after it", apps, err)
	}

	if _, err := m.NativeApps("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("NativeApps of a worker that is not there = %v, want ErrNotFound", err)
	}
	// A stopped worker has no screen, so its screen is not "empty", and nothing can be opened on it.
	if m.NativeScreenEmpty("ada") {
		t.Error("a worker that is not running was reported as an empty screen")
	}
	if err := m.OpenNativeProgram("ada", apps[1]); !errors.Is(err, ErrNotRunning) {
		t.Errorf("opening a program on a stopped worker = %v, want ErrNotRunning", err)
	}
}

// shortcutFile is a .lnk as Windows writes one: the header, the target as a
// LinkInfo block (with its Unicode path when wide), and the counted strings.
func shortcutFile(target, args string, wide bool) []byte {
	le16 := func(v int) []byte { return []byte{byte(v), byte(v >> 8)} }
	le32 := func(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	utf := func(s string) []byte {
		var out []byte
		for _, r := range s {
			out = append(out, le16(int(r))...)
		}
		return out
	}

	flags := 1<<0 | 1<<1 | 1<<2 | 1<<4 | 1<<7 // an ID list, LinkInfo, a name, a working folder; Unicode strings
	if args != "" {
		flags |= 1 << 5
	}

	data := make([]byte, 0x4C)
	copy(data, le32(0x4C))
	copy(data[20:], le32(flags))

	idList := []byte{1, 2, 3, 4, 0, 0}
	data = append(data, le16(len(idList))...)
	data = append(data, idList...)

	header := 0x1C
	if wide {
		header = 0x24
	}
	ansi := append([]byte(target), 0)
	body := append(append([]byte{}, ansi...), 0) // the path, then an empty suffix
	info := append(le32(0), le32(header)...)
	info = append(info, le32(1)...)                // VolumeIDAndLocalBasePath
	info = append(info, le32(0)...)                // volume ID: none
	info = append(info, le32(header)...)           // the path
	info = append(info, le32(0)...)                // no network link
	info = append(info, le32(header+len(ansi))...) // the suffix
	if wide {
		info = append(info, le32(header+len(body))...) // the path again, in Unicode
		info = append(info, le32(0)...)
		body = append(body, append(utf(target), 0, 0)...)
	}
	info = append(info, body...)
	copy(info, le32(len(info)))
	data = append(data, info...)

	for _, s := range []string{"a comment", `C:\Somewhere`, args} {
		if s == "" {
			continue
		}
		data = append(data, le16(len([]rune(s)))...)
		data = append(data, utf(s)...)
	}

	return data
}

// The shortcut Chrome puts on a person's desktop for one of its profiles is
// Chrome by neither its name nor its file name. Found live: it was the only
// browser picked for a worker, the worker was taken to have none, and its
// agent opened Edge - where the person had not signed in.
func TestAShortcutFileToABrowserIsTheWorkersBrowser(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	chrome := `C:\Program Files\Google\Chrome\Application\chrome.exe`
	profile := write("Koustubh - Chrome.lnk", shortcutFile(chrome, `--profile-directory="Profile 2"`, true))
	plain := write("Internet.lnk", shortcutFile(`C:\Apps\Brave\brave.exe`, "", false))
	webApp := write("YouTube.lnk", shortcutFile(chrome, `--profile-directory=Default --app-id=agimnkijcaahngcdmfeangaknmldooml`, true))
	other := write("Editor.lnk", shortcutFile(`C:\Apps\Code.exe`, "", true))
	broken := write("Broken.lnk", []byte("not a shortcut"))

	if target, args, ok := readShortcut(profile); !ok || target != chrome || args != `--profile-directory="Profile 2"` {
		t.Fatalf("the shortcut read as %q %q (%v)", target, args, ok)
	}

	for _, c := range []struct {
		app  NativeApp
		want pickedBrowser
	}{
		{NativeApp{Name: "Koustubh - Chrome", Kind: NativeAppProgram, Target: profile}, pickedBrowser{"chrome", chrome, "Profile 2"}},
		{NativeApp{Name: "Internet", Kind: NativeAppProgram, Target: plain}, pickedBrowser{"brave", `C:\Apps\Brave\brave.exe`, ""}},
		// A page of the browser, another program and a file that is no shortcut are not it.
		{NativeApp{Name: "YouTube", Kind: NativeAppProgram, Target: webApp}, pickedBrowser{}},
		{NativeApp{Name: "Editor", Kind: NativeAppProgram, Target: other}, pickedBrowser{}},
		{NativeApp{Name: "Broken", Kind: NativeAppProgram, Target: broken}, pickedBrowser{}},
		{NativeApp{Name: "Gone", Kind: NativeAppProgram, Target: filepath.Join(dir, "gone.lnk")}, pickedBrowser{}},
		// Arguments given with the shortcut are what it is started with, not its own.
		{NativeApp{Name: "Mail", Kind: NativeAppProgram, Target: profile, Args: "--app=https://mail.example.com"}, pickedBrowser{}},
	} {
		got, ok := browserOf(c.app)
		if !ok {
			got = pickedBrowser{}
		}
		if got != c.want {
			t.Errorf("browserOf(%s) = %+v, want %+v", c.app.Name, got, c.want)
		}
	}

	// The agent is told the browser, the profile the shortcut opens, and -
	// once the worker's browser has a profile folder from its icon - that folder.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	m := nativeForTest(t)
	meta := nativeMeta{ID: "ada", Name: "Ada", Harness: HarnessTheBot, Brain: BrainClaude,
		Apps: []NativeApp{{ID: "win_k", Name: "Koustubh - Chrome", Kind: NativeAppProgram, Target: profile}}}
	writeWorker(t, m, meta)
	file := filepath.Join(m.native.home("ada"), ".decurix", browserChoiceFile)

	told := func() map[string]string {
		t.Helper()
		m.native.noteBrowser(meta)
		var choice map[string]string
		data, err := os.ReadFile(file)
		if err != nil || json.Unmarshal(data, &choice) != nil {
			t.Fatalf("the agent was not told its browser: %v (%s)", err, data)
		}
		return choice
	}

	choice := told()
	if choice["kind"] != "chrome" || choice["exe"] != chrome || choice["profile_directory"] != "Profile 2" || choice["user_data_dir"] != "" {
		t.Errorf("the agent was told %v", choice)
	}

	kept := filepath.Join(os.Getenv("LOCALAPPDATA"), "Decurix", "hidden-profiles", "decurix_w_ada-chrome")
	if err := os.MkdirAll(kept, 0o755); err != nil {
		t.Fatal(err)
	}
	if choice = told(); choice["user_data_dir"] != kept {
		t.Errorf("the agent was told %v, want the profile its browser already has: %s", choice, kept)
	}
}

// A worker's agent is told every shortcut its screen has: what it opens by
// name for a person (so a program opened from an icon is a step of a
// recording) and what a replay opens when a step's window is not there.
func TestAWorkersAgentIsToldItsShortcuts(t *testing.T) {
	dir := t.TempDir()
	charmap := filepath.Join(dir, "Character Map.lnk")
	if err := os.WriteFile(charmap, shortcutFile(`C:\Windows\System32\charmap.exe`, "", true), 0o644); err != nil {
		t.Fatal(err)
	}

	m := nativeForTest(t)
	meta := nativeMeta{ID: "ada", Name: "Ada", Harness: HarnessTheBot, Brain: BrainClaude, Apps: []NativeApp{
		{ID: "win_chrome", Name: "Google Chrome", Kind: NativeAppProgram, Target: `C:\Apps\chrome.exe`},
		{ID: "win_charmap", Name: "Character Map", Kind: NativeAppProgram, Target: charmap},
		{ID: "win_notepad", Name: "Notepad", Kind: NativeAppProgram, Target: `shell:AppsFolder\Microsoft.WindowsNotepad_8wekyb3d8bbwe!App`},
		{ID: "web_mail", Name: "Mail", Kind: NativeAppWeb, Target: "https://mail.example.com"},
	}}
	writeWorker(t, m, meta)
	m.native.noteApps(meta)

	var told []agentApp
	data, err := os.ReadFile(filepath.Join(m.native.home("ada"), ".decurix", workerAppsFile))
	if err != nil || json.Unmarshal(data, &told) != nil {
		t.Fatalf("the agent was not told its shortcuts: %v (%s)", err, data)
	}

	want := []agentApp{
		{ID: "win_chrome", Name: "Google Chrome", Kind: "browser", Exe: `C:\Apps\chrome.exe`},
		{ID: "win_charmap", Name: "Character Map", Kind: "program", Exe: `C:\Windows\System32\charmap.exe`},
		{ID: "win_notepad", Name: "Notepad", Kind: "program"},
		{ID: "web_mail", Name: "Mail", Kind: "web", Target: "https://mail.example.com"},
	}
	if len(told) != len(want) {
		t.Fatalf("told %+v, want %+v", told, want)
	}
	for i := range want {
		if told[i] != want[i] {
			t.Errorf("shortcut %d: told %+v, want %+v", i, told[i], want[i])
		}
	}

	// The agent may have a program shortcut started, and only that: never its
	// browser (which it starts itself, with its DevTools port), a web
	// shortcut, or anything the worker was not given.
	for _, id := range []string{"win_chrome", "web_mail", "win_paint"} {
		if err := m.OpenWorkerProgram("ada", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenWorkerProgram(%s) = %v, want ErrNotFound", id, err)
		}
	}
	if err := m.OpenWorkerProgram("ada", "win_charmap"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a worker that is not running had a program opened on it: %v", err)
	}
}
