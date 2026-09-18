package desktop

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectHostApps(t *testing.T) {
	items := DetectHostApps()
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

func TestPackDynamicApp(t *testing.T) {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	app := HostAppItem{
		ID:          "win_spotify",
		Name:        "Spotify Music",
		Category:    "Media & Entertainment",
		Description: "Spotify Windows desktop app",
		HostCmd:     `cmd.exe /c start "" "shell:AppsFolder\Spotify.exe"`,
		Icon:        "audio-x-generic",
	}

	if err := packDynamicApp(tw, app); err != nil {
		t.Fatalf("packDynamicApp failed: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(buf)
	foundFiles := make(map[string]bool)
	var desktopContent string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		foundFiles[hdr.Name] = true
		if hdr.Name == "Desktop/spotify-music-host.desktop" {
			data, _ := io.ReadAll(tr)
			desktopContent = string(data)
		}
	}

	if !foundFiles["Desktop/spotify-music-host.desktop"] {
		t.Errorf("expected Desktop/spotify-music-host.desktop to be packed")
	}
	if !foundFiles[".local/share/applications/spotify-music-host.desktop"] {
		t.Errorf("expected .local/share/applications/spotify-music-host.desktop to be packed")
	}
	if !strings.Contains(desktopContent, "Exec=/home/headless/.local/bin/host-run cmd.exe /c start") {
		t.Errorf("unexpected desktop content: %s", desktopContent)
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
		{"Flowera repo recursion", "/home/user/flowera/cmd/flowera", true, true},
		{"Migration toolkit recursion", "/home/user/migration-toolkit", true, true},
		{"Node modules", "/home/user/.config/Code/node_modules", true, true},
		{"Cookies DB", "/home/user/.config/google-chrome/Default/Cookies", false, false},
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
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	if err := packHostBridgeLaunchers(tw); err != nil {
		t.Fatalf("packHostBridgeLaunchers failed: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	tr := tar.NewReader(buf)
	foundFiles := make(map[string]bool)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		foundFiles[hdr.Name] = true

		if strings.HasSuffix(hdr.Name, ".desktop") || strings.HasPrefix(hdr.Name, ".local/bin/") {
			if hdr.Mode&0111 == 0 {
				t.Errorf("file %s expected executable bit set, got mode %o", hdr.Name, hdr.Mode)
			}
		}
	}

	expected := []string{
		".local/bin/host-run",
		".local/bin/host-open",
		".local/bin/host-clip",
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
	}

	for _, exp := range expected {
		if !foundFiles[exp] {
			t.Errorf("expected %s in host bridge tar archive", exp)
		}
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

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	err = writeDirToTar(tw, tmpDir, ".config/app", isCacheOrTransient)
	if err != nil {
		t.Fatalf("writeDirToTar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(buf)
	foundFiles := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		foundFiles[hdr.Name] = true
	}

	if !foundFiles[".config/app/Cookies"] {
		t.Errorf("expected Cookies to be preserved")
	}
	if !foundFiles[".config/app/Preferences"] {
		t.Errorf("expected Preferences to be preserved")
	}
	if foundFiles[".config/app/Cache/data_0"] {
		t.Errorf("Cache/data_0 should have been stripped!")
	}
}
