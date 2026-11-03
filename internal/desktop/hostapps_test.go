package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// testPack is the desktop the launcher tests write into.
var testPack = packContext{DesktopID: "work-vm", Secret: []byte("0123456789abcdef0123456789abcdef")}

// useHostChecker installs check as the host's verdict for the length of a test.
func useHostChecker(t *testing.T, check HostAppChecker) {
	t.Helper()
	SetHostAppChecker(check)
	t.Cleanup(func() { SetHostAppChecker(nil) })
}

// packedFile is one entry of an archive a test packed.
type packedFile struct {
	hdr  *tar.Header
	data string
}

// pack runs fill into a fresh tar archive and returns its entries by name: what copying it into a desktop would put there.
func pack(t *testing.T, fill func(*tar.Writer) error) map[string]packedFile {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := fill(tw); err != nil {
		t.Fatalf("packing: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	files := map[string]packedFile{}
	tr := tar.NewReader(buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = packedFile{hdr, string(data)}
	}
}

// packApp is pack of one app's launcher for testPack.
func packApp(t *testing.T, app HostAppItem) map[string]packedFile {
	t.Helper()
	return pack(t, func(tw *tar.Writer) error { return packDynamicApp(tw, app, testPack) })
}

// allowEverything is a host that says every program can run hidden.
func allowEverything(_ context.Context, targets []winhost.Target) (map[winhost.Target]string, error) {
	out := make(map[winhost.Target]string, len(targets))
	for _, t := range targets {
		out[t] = ""
	}
	return out, nil
}

func TestDetectHostApps(t *testing.T) {
	useHostChecker(t, allowEverything)
	items := DetectHostAppsForce(true)
	if len(items) == 0 {
		t.Fatal("expected non-empty host app list")
	}

	foundBridge := false
	foundEdge := false

	for _, item := range items {
		if !item.Detected {
			t.Errorf("expected ONLY detected apps, but found undetected item: %s (%s)", item.Name, item.ID)
		}
		if item.ID == "host_bridge" {
			foundBridge = true
		}
		if item.ID == "edge" || item.ID == "win_microsoft_edge" || strings.Contains(strings.ToLower(item.Name), "edge") {
			foundEdge = true
		}
	}

	if !foundBridge {
		t.Errorf("expected host_bridge in DetectHostApps list")
	}
	if IsWSLOrWindows() && !foundEdge {
		t.Errorf("expected Edge to be detected on WSL/Windows host")
	}

	t.Logf("Detected %d total verified apps across host system", len(items))
	detectedCount := 0
	for _, item := range items {
		if item.Detected {
			detectedCount++
			t.Logf("  [READY] %s (%s) - %s [cmd: %s]", item.Name, item.Category, item.Description, item.HostCmd)
		}
	}
	t.Logf("Total ready/detected: %d / %d", detectedCount, len(items))
}

func TestParseGetStartAppsJSON(t *testing.T) {
	jsonArray := []byte(`[
		{"Name":"Microsoft Edge","AppID":"Microsoft.MicrosoftEdge.Stable_8wekyb3d8bbwe!App"},
		{"Name":"YouTube","AppID":"Chrome._crx_agimnkijcaiengbajagbfclojfljnakf"},
		{"Name":"Spotify","AppID":"Spotify.exe"}
	]`)

	apps := parseGetStartAppsJSON(jsonArray)
	if len(apps) != 3 {
		t.Fatalf("expected 3 apps, got %d", len(apps))
	}
	if apps[0].Name != "Microsoft Edge" || apps[1].Name != "YouTube" || apps[2].Name != "Spotify" {
		t.Errorf("unexpected parsed apps: %+v", apps)
	}

	// Single object format
	singleJSON := []byte(`{"Name":"Discord","AppID":"com.squirrel.Discord.Discord"}`)
	singleApps := parseGetStartAppsJSON(singleJSON)
	if len(singleApps) != 1 || singleApps[0].Name != "Discord" {
		t.Errorf("unexpected single parsed app: %+v", singleApps)
	}
}

func TestWSLToWindowsPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/mnt/c/ProgramData/Microsoft/Windows/Start Menu/Programs", `C:\ProgramData\Microsoft\Windows\Start Menu\Programs`},
		{"/mnt/c/Users/Administrator/AppData/Local/Microsoft/Edge/User Data", `C:\Users\Administrator\AppData\Local\Microsoft\Edge\User Data`},
		{"/c/Users/User/AppData", `C:\Users\User\AppData`},
		{"/mnt/d/Games/Steam", `D:\Games\Steam`},
	}

	for _, tc := range tests {
		res := wslToWindowsPath(tc.input)
		if res != tc.expected {
			t.Errorf("wslToWindowsPath(%q) = %q; want %q", tc.input, res, tc.expected)
		}
	}
}

func TestCleanAppShortcutName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Visual Studio Code (64-bit)", "Visual Studio Code"},
		{"Spotify - Shortcut", "Spotify"},
		{"Google Chrome (32-bit) - Shortcut", "Google Chrome"},
		{"Microsoft Edge", "Microsoft Edge"},
	}

	for _, tc := range tests {
		res := cleanAppShortcutName(tc.input)
		if res != tc.expected {
			t.Errorf("cleanAppShortcutName(%q) = %q; want %q", tc.input, res, tc.expected)
		}
	}
}

func TestChromeIsDiscoveredWithoutAStartMenuEntry(t *testing.T) {
	drive := t.TempDir()
	exe := filepath.Join(drive, "Program Files", "Google", "Chrome", "Application", "chrome.exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("test"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECURIX_HOST_MNT_C", drive)

	apps := scanInstalledWindowsBrowsers(map[string]bool{})
	found := false
	for _, app := range apps {
		if app.Name == "Google Chrome" && strings.Contains(app.HostCmd, `Google\Chrome\Application\chrome.exe`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("standard Chrome install was not discovered: %+v", apps)
	}
}

func TestPackDynamicApp(t *testing.T) {
	app := HostAppItem{
		ID:          "win_spotify",
		Name:        "Spotify Music",
		Category:    "Media & Entertainment",
		Description: "Spotify Windows desktop app",
		HostCmd:     `cmd.exe /c start explorer.exe shell:AppsFolder\Spotify.exe`,
		Icon:        "audio-x-generic",
	}

	files := packApp(t, app)
	for _, name := range []string{".local/bin/spotify-music-host", "Desktop/spotify-music-host.desktop", ".local/share/applications/spotify-music-host.desktop"} {
		if _, ok := files[name]; !ok {
			t.Errorf("expected %s to be packed", name)
		}
	}
	desktopContent := files["Desktop/spotify-music-host.desktop"].data
	wrapperContent := files[".local/bin/spotify-music-host"].data
	if !strings.Contains(desktopContent, "Exec=/home/headless/.local/bin/spotify-music-host %U") {
		t.Errorf("unexpected desktop content: %s", desktopContent)
	}
	if !strings.Contains(wrapperContent, "/host-gui/view?") || !strings.Contains(wrapperContent, "id=work-vm-spotify-music") ||
		!strings.Contains(wrapperContent, "token=") ||
		!strings.Contains(wrapperContent, "target=shell%3AAppsFolder%5CSpotify.exe") {
		t.Errorf("a Windows app must open the host-gui viewer with its target, got: %s", wrapperContent)
	}
	if strings.Contains(wrapperContent, "host-run") {
		t.Errorf("a Windows app must not be launched visibly on the host through host-run: %s", wrapperContent)
	}
}

func TestCacheExclusion(t *testing.T) {
	testCases := []struct {
		name     string
		path     string
		isDir    bool
		expected bool
	}{
		{"Normal file", "/home/user/.config/gh/hosts.yml", false, false},
		{"Chrome Cache dir", "/home/user/.config/google-chrome/Default/Cache", true, true},
		{"GPUCache", "/home/user/.config/google-chrome/Default/GPUCache", true, true},
		{"Code Cache", "/home/user/.config/google-chrome/Default/Code Cache", true, true},
		{"Crashpad", "/home/user/.config/google-chrome/Crashpad", true, true},
		{"Temporary file", "/home/user/.config/google-chrome/Default/file.tmp", false, true},
		{"Log file", "/home/user/.config/google-chrome/Default/debug.log", false, true},
		{"Decurix repo recursion", "/home/user/decurix/cmd/decurix", true, true},
		{"Migration toolkit recursion", "/home/user/migration-toolkit", true, true},
		{"Node modules", "/home/user/.config/Code/node_modules", true, true},
		{"Cookies DB", "/home/user/.config/google-chrome/Default/Cookies", false, false},
		{"Worker profile under Decurix", "/home/user/Decurix/workers/jaga/home/.config/Default/Cookies", false, false},
		{"Login Data DB", "/home/user/.config/google-chrome/Default/Login Data", false, false},
	}

	for _, tc := range testCases {
		res := isCacheOrTransient(filepath.Base(tc.path), tc.path, tc.isDir)
		if res != tc.expected {
			t.Errorf("isCacheOrTransient(%q, %q, %v) = %v; want %v", filepath.Base(tc.path), tc.path, tc.isDir, res, tc.expected)
		}
	}
}

func TestPackHostBridgeLaunchers(t *testing.T) {
	files := pack(t, packHostBridgeLaunchers)
	for name, f := range files {
		if strings.HasSuffix(name, ".desktop") || strings.HasPrefix(name, ".local/bin/") {
			if f.hdr.Mode&0111 == 0 {
				t.Errorf("file %s expected executable bit set, got mode %o", name, f.hdr.Mode)
			}
		}
	}

	expected := []string{
		".local/bin/host-run",
	}

	for _, exp := range expected {
		if _, ok := files[exp]; !ok {
			t.Errorf("expected %s in host bridge tar archive", exp)
		}
	}

	unexpected := []string{
		// a link opened in the person's browser is signed into a launcher too, and nothing sets their clipboard
		".local/bin/host-open",
		".local/bin/host-clip",
		// generic "run anything on the host" wrappers: host commands are signed into launchers now
		".local/bin/win-run",
		".local/bin/mac-run",
		".local/bin/chrome-host",
		".local/bin/edge-host",
		".local/bin/youtube-host",
		".local/bin/code-host",
		".local/bin/terminal-host",
		"Desktop/chrome-host.desktop",
		"Desktop/edge-host.desktop",
		"Desktop/youtube-host.desktop",
		"Desktop/code-host.desktop",
		"Desktop/terminal-host.desktop",
		".local/share/applications/chrome-host.desktop",
		".local/share/applications/edge-host.desktop",
		".local/share/applications/youtube-host.desktop",
		".local/share/applications/code-host.desktop",
		".local/share/applications/terminal-host.desktop",
	}

	for _, unexp := range unexpected {
		if _, ok := files[unexp]; ok {
			t.Errorf("did NOT expect %s in host bridge tar archive (should only be packed when explicitly selected)", unexp)
		}
	}
}

func TestSelectedAppsPackaging(t *testing.T) {
	defs := getKnownAppDefs()
	defMap := make(map[string]knownAppDef)
	for _, d := range defs {
		defMap[d.id] = d
	}

	testCases := []struct {
		id           string
		expectedFile string
	}{
		{"claude", "Desktop/claude-host.desktop"},
		{"chatgpt", "Desktop/chatgpt-host.desktop"},
	}

	for _, tc := range testCases {
		t.Run(tc.id, func(t *testing.T) {
			def, ok := defMap[tc.id]
			if !ok {
				t.Fatalf("missing def for %s", tc.id)
			}
			files := pack(t, func(tw *tar.Writer) error { return def.packFn(tw, []string{}, packContext{}) })
			if _, found := files[tc.expectedFile]; !found {
				t.Errorf("expected %s to be packed when %s is selected", tc.expectedFile, tc.id)
			}
		})
	}
}

func TestWriteDirToTar(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hostapps_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create dummy files: cookies, cache, config
	_ = os.WriteFile(filepath.Join(tmpDir, "Cookies"), []byte("auth_cookie_token=abc123xyz"), 0600)
	_ = os.MkdirAll(filepath.Join(tmpDir, "Cache"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "Cache", "data_0"), []byte("cache_blob_should_be_stripped"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "Preferences"), []byte(`{"logged_in": true}`), 0644)

	files := pack(t, func(tw *tar.Writer) error { return writeDirToTar(tw, tmpDir, ".config/app", isCacheOrTransient) })

	if _, ok := files[".config/app/Cookies"]; !ok {
		t.Errorf("expected Cookies to be preserved")
	}
	if _, ok := files[".config/app/Preferences"]; !ok {
		t.Errorf("expected Preferences to be preserved")
	}
	if _, ok := files[".config/app/Cache/data_0"]; ok {
		t.Errorf("Cache/data_0 should have been stripped!")
	}
}

func TestGetWebAppURLForApp(t *testing.T) {
	tests := []struct {
		name     string
		cmdOrId  string
		expected string
	}{
		{"Claude", "Claude_pzs8sxrjxfjjc!Claude", "https://claude.ai"},
		{"ChatGPT", "OpenAI.Codex_2p2nqsd0c76g0!App", "https://chatgpt.com"},
		{"Chat GPT", "chatgpt.exe", "https://chatgpt.com"},
		{"Microsoft Copilot", "Microsoft.Copilot_8wekyb3d8bbwe!App", "https://copilot.microsoft.com"},
		{"YouTube", "Chrome._crx_agimnkijcaiengbajagbfclojfljnakf", "https://www.youtube.com"},
		{"WhatsApp", "5319275A.WhatsAppDesktop_cv1g1gvanyjgm!App", "https://web.whatsapp.com"},
		{"Discord", "com.squirrel.Discord.Discord", "https://discord.com/app"},
		{"Slack", "slack.exe", "https://app.slack.com"},
		{"Custom Web App", "https://example.com/myapp", "https://example.com/myapp"},
		{"Start URL", "cmd.exe /c start https://linear.app", "https://linear.app"},
		{"Regular Windows App", "notepad.exe", ""},
		{"Calculator", "Microsoft.WindowsCalculator_8wekyb3d8bbwe!App", ""},
	}

	for _, tc := range tests {
		got := getWebAppURLForApp(tc.name, tc.cmdOrId)
		if got != tc.expected {
			t.Errorf("getWebAppURLForApp(%q, %q) = %q; want %q", tc.name, tc.cmdOrId, got, tc.expected)
		}
	}
}

func TestPackDynamicAppWebAppMode(t *testing.T) {
	app := HostAppItem{
		ID:          "win_claude",
		Name:        "Claude",
		Category:    "Productivity",
		Description: "AI assistant",
		HostCmd:     `cmd.exe /c start explorer.exe shell:AppsFolder\Claude_pzs8sxrjxfjjc!Claude`,
	}

	wrapper, foundWrapper := packApp(t, app)[".local/bin/claude-host"]
	wrapperContent := wrapper.data
	if !foundWrapper {
		t.Fatal("expected .local/bin/claude-host to be packed")
	}
	if !strings.Contains(wrapperContent, `APP='https://claude.ai'`) || !strings.Contains(wrapperContent, `--app="$APP"`) {
		t.Errorf("expected wrapper to use chromium-browser --app mode for Claude, got:\n%s", wrapperContent)
	}
}

func TestPackDynamicAppSVGIcon(t *testing.T) {
	app := HostAppItem{
		ID:          "win_edge",
		Name:        "Microsoft Edge",
		Category:    "Browsers & Web",
		Description: "Web browser",
		HostCmd:     `cmd.exe /c start msedge.exe`,
	}

	files := packApp(t, app)
	svg, foundSVG := files[".local/share/icons/hicolor/scalable/apps/microsoft-edge.svg"]
	if foundSVG && !strings.Contains(svg.data, "<svg") {
		t.Errorf("expected valid SVG content, got:\n%s", svg.data)
	}
	desktop, foundDesktop := files["Desktop/microsoft-edge-host.desktop"]
	desktopContent := desktop.data

	if !foundSVG {
		t.Errorf("expected embedded SVG icon to be packed at .local/share/icons/hicolor/scalable/apps/microsoft-edge.svg")
	}
	if !foundDesktop {
		t.Fatalf("expected Desktop/microsoft-edge-host.desktop to be packed")
	}
	if !strings.Contains(desktopContent, "Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/microsoft-edge.svg") {
		t.Errorf("expected desktop entry Icon to reference scalable svg icon, got:\n%s", desktopContent)
	}
}

func TestPackHostBridgeLaunchersIncludesDisplay(t *testing.T) {
	hostRunContent := pack(t, packHostBridgeLaunchers)[".local/bin/host-run"].data

	if !strings.Contains(hostRunContent, `body["display"] = os.environ.get("DISPLAY", ":1")`) {
		t.Errorf("expected host-run to send container DISPLAY in payload, got:\n%s", hostRunContent)
	}
	if strings.Contains(hostRunContent, "ssh ") {
		t.Errorf("host-run must not have an unsigned ssh fallback, got:\n%s", hostRunContent)
	}
}

// TestHostRunWorksOnAnImageWithoutCurlOrLocalBinOnPath pins the two things that made a copied
// desktop app do nothing when clicked: the wrappers ran "host-run" by bare name (~/.local/bin is
// not on the basic image's PATH, so the shell said "host-run: not found"), and host-run itself
// posted with curl, which the desktop images do not all ship.
func TestHostRunWorksOnAnImageWithoutCurlOrLocalBinOnPath(t *testing.T) {
	app := HostAppItem{ID: "win_notepad", Name: "Notepad", HostCmd: `cmd.exe /c start "" "C:\Notepad.lnk"`}
	files := pack(t, func(tw *tar.Writer) error {
		if err := packHostBridgeLaunchers(tw); err != nil {
			return err
		}
		return packDynamicApp(tw, app, testPack)
	})

	scripts := 0
	for name, f := range files {
		if !strings.HasPrefix(name, ".local/bin/") {
			continue
		}
		content := f.data
		scripts++
		if strings.Contains(content, "curl") && !strings.Contains(content, "python3 rather than curl") {
			t.Errorf("%s calls curl, which the desktop images do not all have:\n%s", name, content)
		}
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "exec host-run") {
				t.Errorf("%s runs host-run by bare name, which is not on PATH in the basic image: %q", name, line)
			}
		}
	}
	if scripts == 0 {
		t.Fatal("no scripts packed")
	}
}

// TestShellWordsKeepsWindowsBackslashes pins the bug where bash ate the backslash in
// shell:AppsFolder\<AppUserModelID>, so a copied Store app asked the host for a name that does not exist.
func TestShellWordsKeepsWindowsBackslashes(t *testing.T) {
	cases := map[string]string{
		`cmd.exe /c start explorer.exe shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`: `cmd.exe /c start explorer.exe 'shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App'`,
		`cmd.exe /c start "" "C:\Users\me\My Apps\Code.exe"`:                                           `cmd.exe /c start '' 'C:\Users\me\My Apps\Code.exe'`,
		`open -a "Google Chrome"`:                  `open -a 'Google Chrome'`,
		`cmd.exe /c start "" "C:\it's here\a.exe"`: `cmd.exe /c start '' 'C:\it'\''s here\a.exe'`,
	}
	for in, want := range cases {
		if got := shellWords(in); got != want {
			t.Errorf("shellWords(%q)\n got  %s\n want %s", in, got, want)
		}
	}
}

// requireBash skips a test that runs one of the launcher scripts. Those are bash scripts for the Linux desktop they are
// copied into; there is nothing to run them with on a Windows machine, whatever bash WSL puts on its PATH.
func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("launchers are bash scripts for the Linux VM")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

// TestDynamicAppWrapperRunsUnderBashWithBackslashesIntact runs the generated wrapper with a stub
// host-run and checks the argument the bridge would send, byte for byte.
func TestDynamicAppWrapperRunsUnderBashWithBackslashesIntact(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	app := HostAppItem{ID: "win_calc", Name: "Calc", HostCmd: `winapp --profile "C:\Program Files\App" --name "it's"`}
	wrapper := strings.ReplaceAll(packApp(t, app)[".local/bin/calc-host"].data, "/home/headless/.local/bin/host-run", filepath.Join(dir, "host-run"))
	stub := "#!/usr/bin/env bash\nfor a in \"$@\"; do printf '[%s]' \"$a\"; done\n"
	if err := os.WriteFile(filepath.Join(dir, "host-run"), []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calc-host"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(dir, "calc-host")).Output()
	if err != nil {
		t.Fatal(err)
	}
	tok := winhost.ExecToken(testPack.Secret, testPack.DesktopID, "winapp", []string{"--profile", `C:\Program Files\App`, "--name", "it's"})
	want := `[signed][` + tok + `][winapp][--profile][C:\Program Files\App][--name][it's]`
	if string(out) != want {
		t.Errorf("host-run received %s, want %s", out, want)
	}
}

func TestWindowsTarget(t *testing.T) {
	cases := []struct {
		in, target, args string
		ok               bool
	}{
		{`cmd.exe /c start "" "C:\Users\me\AppData\Local\Programs\Microsoft VS Code\Code.exe"`, `C:\Users\me\AppData\Local\Programs\Microsoft VS Code\Code.exe`, "", true},
		{`cmd.exe /c start "" "C:\Users\me\Desktop\Chrome.lnk"`, `C:\Users\me\Desktop\Chrome.lnk`, "", true},
		{`cmd.exe /c start explorer.exe shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`, `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`, "", true},
		{`cmd.exe /c start "" "C:\a.exe" --flag value`, `C:\a.exe`, "--flag value", true},
		{`open -a "Google Chrome"`, "", "", false},
		{`cmd.exe /c dir`, "", "", false},
		{``, "", "", false},
	}
	for _, c := range cases {
		target, args, ok := windowsTarget(c.in)
		if target != c.target || args != c.args || ok != c.ok {
			t.Errorf("windowsTarget(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, target, args, ok, c.target, c.args, c.ok)
		}
	}
}

// hostGUIWrapper is a complete launcher script around hostGUILauncher.
func hostGUIWrapper(pc packContext, slug, title, target, args string) string {
	return "#!/usr/bin/env bash\n" + hostGUILauncher(pc, slug, title, target, args)
}

// TestHostGUIWrapperOpensTheViewerForThisDesktop runs the generated launcher with a stand-in browser first on PATH
// (never a real one: a test must not open windows) and checks the URL it would open.
func TestHostGUIWrapperOpensTheViewerForThisDesktop(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chromium-browser"), []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	script := hostGUIWrapper(testPack, "vs-code", "Visual Studio Code", `C:\Program Files\Code.exe`, `--folder "a b"`)
	wrapper := filepath.Join(dir, "vs-code-host")
	if err := os.WriteFile(wrapper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "CONTROL_PLANE_URL=http://cp.test:8080"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("wrapper failed: %v\n%s", err, script)
	}
	arg := strings.TrimSpace(string(out))
	raw, ok := strings.CutPrefix(arg, "--app=")
	if !ok {
		t.Fatalf("browser was given %q, want an --app= window", arg)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "cp.test:8080" || u.Path != "/host-gui/view" ||
		q.Get("id") != "work-vm-vs-code" || q.Get("desktop") != "work-vm" ||
		q.Get("token") != winhost.Token(testPack.Secret, "work-vm-vs-code", `C:\Program Files\Code.exe`, `--folder "a b"`) || q.Get("title") != "Visual Studio Code" ||
		q.Get("target") != `C:\Program Files\Code.exe` || q.Get("args") != `--folder "a b"` {
		t.Fatalf("viewer URL %s carries the wrong values: %v", raw, q)
	}
}

func TestHostGUIWrapperTellsYouWhenThereIsNoBrowser(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "w")
	os.WriteFile(wrapper, []byte(hostGUIWrapper(testPack, "x", "X", `C:\x.exe`, "")), 0755)
	cmd := exec.Command("/bin/bash", wrapper)
	cmd.Env = []string{"PATH=" + dir} // nothing on it, so no browser is found
	if err := cmd.Run(); err == nil {
		t.Fatal("wrapper reported success with no browser to show the app in")
	}
}

func TestLauncherTokenIsBoundToTheAppItLaunches(t *testing.T) {
	script := hostGUIWrapper(testPack, "chrome", "Chrome", `C:\Program Files\Google\Chrome\chrome.exe`, "")
	other := hostGUIWrapper(testPack, "chrome", "Chrome", `C:\Windows\System32\cmd.exe`, "")
	tokenOf := func(s string) string {
		i := strings.Index(s, "token=")
		return s[i+len("token=") : i+len("token=")+32]
	}
	if tokenOf(script) == tokenOf(other) {
		t.Fatal("two different programs share a launch token: a launcher could be edited to run something else")
	}
	unsigned := hostGUIWrapper(packContext{DesktopID: "work-vm"}, "chrome", "Chrome", `C:\a.exe`, "")
	if strings.Contains(unsigned, "token=") {
		t.Fatal("a launcher written with no secret carries a token")
	}
}

func windowsApp(name, hostCmd string) HostAppItem {
	return HostAppItem{ID: "win_" + sanitizeAppSlug(name), Name: name, Detected: true, AppType: "windows_app", HostCmd: hostCmd}
}

func TestEveryDiscoveredAppIsShownAndOnlyRunnableAppsAreEnabled(t *testing.T) {
	const uwp = `cmd.exe /c start explorer.exe shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`
	const paint = `cmd.exe /c start "" "C:\Windows\System32\mspaint.exe"`
	useHostChecker(t, func(_ context.Context, targets []winhost.Target) (map[winhost.Target]string, error) {
		out := map[winhost.Target]string{}
		for _, tg := range targets {
			if strings.Contains(tg.Target, "Calculator") {
				out[tg] = "This is a Universal Windows (UWP) app"
			} else {
				out[tg] = ""
			}
		}
		return out, nil
	})

	got := vetHostApps([]HostAppItem{
		windowsApp("Calculator", uwp),
		windowsApp("Paint", paint),
	})
	if len(got) != 2 || got[0].Name != "Calculator" || got[0].Mode != ModeUnavailable ||
		!strings.Contains(got[0].UnavailableReason, "Universal Windows") ||
		got[1].Name != "Paint" || got[1].Mode != ModeHostGUI {
		t.Fatalf("discovered %+v, want unavailable Calculator and runnable Paint", got)
	}
}

func TestAnUnverifiedAppStaysVisibleButDisabled(t *testing.T) {
	const paint = `cmd.exe /c start "" "C:\Windows\System32\mspaint.exe"`
	apps := []HostAppItem{windowsApp("Paint", paint)}

	useHostChecker(t, nil)
	if got := vetHostApps(apps); len(got) != 1 || got[0].Mode != ModeUnavailable || got[0].UnavailableReason == "" {
		t.Errorf("with no way to ask the host, discovery returned %+v", got)
	}

	// A helper that dies partway keeps what it answered; the rest are not promised.
	useHostChecker(t, func(context.Context, []winhost.Target) (map[winhost.Target]string, error) {
		return nil, errors.New("the host helper ended")
	})
	if got := vetHostApps(apps); len(got) != 1 || got[0].Mode != ModeUnavailable {
		t.Errorf("after the host helper failed, discovery returned %+v", got)
	}
}

func TestAWebServiceOpensInTheVMWithoutAskingTheHost(t *testing.T) {
	asked := false
	useHostChecker(t, func(context.Context, []winhost.Target) (map[winhost.Target]string, error) {
		asked = true
		return nil, nil
	})
	got := vetHostApps([]HostAppItem{windowsApp("Claude", `cmd.exe /c start explorer.exe shell:AppsFolder\Anthropic.Claude_x!Claude`)})
	if len(got) != 1 || got[0].Mode != ModeWeb {
		t.Fatalf("offered %+v, want Claude opened as its web version", got)
	}
	if asked {
		t.Error("the host was asked about an app that never runs there")
	}
}

func TestAppsThatNeedSoundAreShownButDisabled(t *testing.T) {
	useHostChecker(t, allowEverything)
	got := vetHostApps([]HostAppItem{
		windowsApp("Spotify", `cmd.exe /c start "" "C:\Users\a\AppData\Roaming\Spotify\Spotify.exe"`),
		windowsApp("VLC media player", `cmd.exe /c start "" "C:\Program Files\VideoLAN\VLC\vlc.exe"`),
	})
	if len(got) != 2 || got[0].Mode != ModeUnavailable || got[1].Mode != ModeUnavailable {
		t.Errorf("audio-only apps should remain visible but disabled: %+v", got)
	}
}

func TestBrowserDescriptionPromisesTheSeededSignedInProfile(t *testing.T) {
	useHostChecker(t, allowEverything)
	got := vetHostApps([]HostAppItem{windowsApp("Google Chrome", `cmd.exe /c start "" "C:\Program Files\Google\Chrome\chrome.exe"`)})
	if len(got) != 1 || got[0].Mode != ModeHostGUI || !strings.Contains(got[0].Description, "signed-in browser profile") {
		t.Fatalf("Chrome discovery did not describe its profile import: %+v", got)
	}
}

func TestForcingDiscoveryForgetsTheWindowsStartAppsSnapshot(t *testing.T) {
	startAppsMu.Lock()
	startAppsCached = []rawStartApp{{Name: "Old App", AppID: "old"}}
	startAppsQueried = true
	startAppsMu.Unlock()
	forgetStartApps()
	startAppsMu.RLock()
	defer startAppsMu.RUnlock()
	if startAppsQueried || startAppsCached != nil {
		t.Fatalf("cached Start Apps survived a forced refresh: queried=%v apps=%+v", startAppsQueried, startAppsCached)
	}
}

func TestTheHostIsOnlyAskedAboutAnAppOnce(t *testing.T) {
	calls := 0
	useHostChecker(t, func(ctx context.Context, targets []winhost.Target) (map[winhost.Target]string, error) {
		calls++
		return allowEverything(ctx, targets)
	})
	apps := []HostAppItem{windowsApp("Paint", `cmd.exe /c start "" "C:\Windows\System32\mspaint.exe"`)}
	vetHostApps(apps)
	vetHostApps(apps)
	if calls != 1 {
		t.Errorf("host asked %d times for the same app, want 1", calls)
	}
	forgetHostVerdicts()
	vetHostApps(apps)
	if calls != 2 {
		t.Errorf("after a rescan host asked %d times in all, want 2", calls)
	}
}

// host-run can only open a signed link and run a signed launcher command. The old generic form
// ("host-run rm -rf ...", and win-run/mac-run which wrapped it) is what let a desktop's agent run anything on the host.
func TestHostRunRefusesToRunAnUnsignedCommand(t *testing.T) {
	requireBash(t)
	script := pack(t, packHostBridgeLaunchers)[".local/bin/host-run"].data
	path := filepath.Join(t.TempDir(), "host-run")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	// Nothing listens on this address, so if the script tried to post anything it would fail differently.
	cmd := exec.Command("bash", path, "cmd.exe", "/c", "calc")
	cmd.Env = append(os.Environ(), "CONTROL_PLANE_URL=http://127.0.0.1:1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("host-run ran an unsigned command: %s", out)
	}
	if !strings.Contains(string(out), "cannot run 'cmd.exe' on the host") {
		t.Errorf("unexpected refusal: %s", out)
	}
}

func TestStaticLaunchersOpenALinkInsteadOfRunningCmd(t *testing.T) {
	for _, def := range getKnownAppDefs() {
		if def.id != "claude" && def.id != "chatgpt" {
			continue
		}
		buf := new(bytes.Buffer)
		tw := tar.NewWriter(buf)
		if err := def.packFn(tw, nil, packContext{}); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		if strings.Contains(buf.String(), "host-run cmd.exe") {
			t.Errorf("%s launcher still asks the host to run cmd.exe", def.id)
		}
		if strings.Contains(buf.String(), "host-run open") {
			t.Errorf("%s launcher opens a link on the host with no signature", def.id)
		}
	}
}

// A web app's launcher opens its own site in the person's browser only with a token for that site and that desktop:
// the control plane opens nothing else (winhost.OpenToken).
func TestWebAppLauncherSignsItsOwnSite(t *testing.T) {
	pc := packContext{DesktopID: "box-a", Secret: []byte("secret")}
	files := pack(t, func(tw *tar.Writer) error { return packWebAppHost(tw, pc, "claude", "Claude", "https://claude.ai") })
	script := files[".local/bin/claude-host"].data
	want := "exec /home/headless/.local/bin/host-run open " + winhost.OpenToken(pc.Secret, "box-a", "https://claude.ai") + " 'https://claude.ai'"
	if !strings.Contains(script, want) {
		t.Fatalf("launcher does not open its site signed: %s", script)
	}
}

// A folder at /mnt/c is not a Windows install: compose always creates that mount, so on a Mac or a Linux machine it is
// an empty folder (or the host's own root) and used to make the control plane believe it ran on Windows.
func TestHasWindowsInstallNeedsWindowsInsideTheDrive(t *testing.T) {
	empty := t.TempDir()
	hostRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostRoot, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if hasWindowsInstall([]string{empty, hostRoot}) {
		t.Fatal("an empty folder and a Linux root were taken for a Windows drive")
	}
	if hasWindowsInstall(nil) {
		t.Fatal("no drives at all were taken for a Windows drive")
	}
	drive := t.TempDir()
	if err := os.MkdirAll(filepath.Join(drive, "Windows", "System32"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasWindowsInstall([]string{empty, drive}) {
		t.Fatal("a drive holding a Windows folder was not recognised")
	}
}

// No app copies the person's own accounts into a worker: a browser profile (cookies, saved passwords), keys, tokens,
// keyrings or dotfiles. Only launchers are packed (review 2026-11-02).
func TestNoAppCopiesTheHostsCredentials(t *testing.T) {
	for _, def := range getKnownAppDefs() {
		switch def.id {
		case "claude", "chatgpt", "host_bridge":
		default:
			t.Errorf("unexpected host app definition %q: only launchers may be copied into a desktop", def.id)
		}
		files := pack(t, func(tw *tar.Writer) error { return def.packFn(tw, nil, packContext{}) })
		for name := range files {
			lower := strings.ToLower(name)
			for _, secret := range []string{".ssh", ".config/google-chrome", ".mozilla", ".config/gh", ".docker", ".config/gcloud", ".azure", "keyrings", "cookies", "login data"} {
				if strings.Contains(lower, secret) {
					t.Errorf("%s packs %s", def.id, name)
				}
			}
		}
	}
}

func TestScanLinuxDesktopFilesAndVet(t *testing.T) {
	tempDir := t.TempDir()
	desktopFile := filepath.Join(tempDir, "sample-editor.desktop")
	content := `[Desktop Entry]
Name=Sample Editor
Comment=A sample editor app
Exec=/usr/bin/sample-editor %F
Icon=accessories-text-editor
Terminal=false
Type=Application
Categories=Utility;TextEditor;
`
	if err := os.WriteFile(desktopFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	meta := parseDesktopFile(desktopFile)
	if meta.Name != "Sample Editor" {
		t.Errorf("parseDesktopFile got Name %q, want 'Sample Editor'", meta.Name)
	}
	if meta.Exec != "/usr/bin/sample-editor %F" {
		t.Errorf("parseDesktopFile got Exec %q", meta.Exec)
	}

	app := HostAppItem{
		ID:            "linux_sample_editor",
		Name:          meta.Name,
		Category:      categorizeAppName(meta.Name),
		Description:   meta.Comment,
		Detected:      true,
		DetectedPaths: []string{desktopFile},
		AppType:       "desktop_app",
		HostCmd:       "/usr/bin/env /usr/bin/sample-editor",
		Icon:          meta.Icon,
	}

	vetted := vetHostApps([]HostAppItem{app})
	if len(vetted) != 1 {
		t.Fatalf("expected 1 vetted app, got %d", len(vetted))
	}
	if vetted[0].Mode != ModeHostGUI {
		t.Errorf("expected ModeHostGUI for desktop_app, got %q (reason: %q)", vetted[0].Mode, vetted[0].UnavailableReason)
	}
}

func TestAPhoneAppOpensThePhoneViewerNotItsWebVersion(t *testing.T) {
	// WhatsApp has a web version too; picked from the phone, it must run on the phone.
	app := HostAppItem{ID: "phone-d1-com.whatsapp", Name: "WhatsApp", Mode: ModePhone, HostCmd: PhoneCmd("d1", "com.whatsapp")}
	files := packApp(t, app)

	wrapper := files[".local/bin/whatsapp-host"].data
	if !strings.Contains(wrapper, "/phone-app/view?") || strings.Contains(wrapper, "web.whatsapp.com") {
		t.Fatalf("the launcher does not open the phone viewer:\n%s", wrapper)
	}

	id := testPack.sessionID("phone-whatsapp")
	token := winhost.Token(testPack.Secret, id, "android:d1", "com.whatsapp")
	if !strings.Contains(wrapper, "token="+token) || !strings.Contains(wrapper, "package=com.whatsapp") {
		t.Fatalf("the launcher is not signed for exactly this app on this phone:\n%s", wrapper)
	}

	if entry := files["Desktop/whatsapp-host.desktop"].data; !strings.Contains(entry, "Name=WhatsApp (Phone)") {
		t.Errorf("launcher entry: %s", entry)
	}
}

// localPageRun runs a local page's launcher with stand-ins first on PATH: curl answers with code (000 is "nothing
// there") and logs its arguments, the browser prints what it was given. It returns what the browser was given and what
// curl was asked.
func localPageRun(t *testing.T, code string) (browser, curls string) {
	t.Helper()
	requireBash(t)
	dir := t.TempDir()
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	stub("curl", `printf '%s\n' "$*" >> "`+dir+`/curl.log"; case "$*" in *http_code*) printf '`+code+`';; esac`)
	stub("chromium-browser", `printf '%s\n' "$@" >> "`+dir+`/browser.log"`)
	stub("sleep", "")
	script := localPageLauncher(testPack, "portal", "Portal", `C:\portal\launcher.exe`, "", "http://host.docker.internal:8000/app")
	wrapper := filepath.Join(dir, "portal-host")
	if err := os.WriteFile(wrapper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "CONTROL_PLANE_URL=http://cp.test:8080"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher failed: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "browser.log"))
	c, _ := os.ReadFile(filepath.Join(dir, "curl.log"))
	return string(b), string(c)
}

// TestALocalWebAppOpensInTheVMsOwnBrowser: a page that answers from the VM is opened there, in an app window, and
// nothing is started on the host.
func TestALocalWebAppOpensInTheVMsOwnBrowser(t *testing.T) {
	browser, curls := localPageRun(t, "200")
	if !strings.Contains(browser, "--app=http://host.docker.internal:8000/app") {
		t.Errorf("browser was given %q, want the page in an app window", browser)
	}
	if strings.Contains(curls, "/api/host-gui/start") {
		t.Errorf("the app was started on the host although its page answered: %s", curls)
	}
}

// TestALocalWebAppWhosePageNeverAnswersIsShownThroughTheViewer: the app is started on the host (its launcher starts the
// server), and a page that still does not answer from the VM falls back to the viewer.
func TestALocalWebAppWhosePageNeverAnswersIsShownThroughTheViewer(t *testing.T) {
	browser, curls := localPageRun(t, "000")
	if !strings.Contains(curls, "http://cp.test:8080/api/host-gui/start") || !strings.Contains(curls, "token=") {
		t.Errorf("the app was not started on the host with its token: %s", curls)
	}
	if !strings.Contains(browser, "--app=http://cp.test:8080/host-gui/view?") {
		t.Errorf("browser was given %q, want the viewer", browser)
	}
	if strings.Contains(browser, "host.docker.internal:8000") {
		t.Errorf("the unreachable page was opened: %q", browser)
	}
}

func TestVMPageURLReachesTheHostsLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:8000":         "http://host.docker.internal:8000",
		"http://127.0.0.1:3000/app?x=1": "http://host.docker.internal:3000/app?x=1",
		"http://localhost/":             "http://host.docker.internal/",
		"https://portal.example.com":    "",
	} {
		if got := vmPageURL(in); got != want {
			t.Errorf("vmPageURL(%q) = %q, want %q", in, got, want)
		}
	}
}
