package desktop

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
)

// HostAppItem describes an application, session profile, or toolchain on the host system.
type HostAppItem struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Category        string   `json:"category"`
	Description     string   `json:"description"`
	Detected        bool     `json:"detected"`
	DetectedPaths   []string `json:"detected_paths,omitempty"`
	DefaultSelected bool     `json:"default_selected"`
	AppType         string   `json:"app_type,omitempty"`
	HostCmd         string   `json:"host_cmd,omitempty"`
	Icon            string   `json:"icon,omitempty"`
	TargetConfigDir string   `json:"target_config_dir,omitempty"`
}

// knownAppDef defines how to probe and migrate a specific application or session.
type knownAppDef struct {
	id              string
	name            string
	category        string
	description     string
	defaultSelected bool
	probeFn         func() []string
	packFn          func(tw *tar.Writer, paths []string) error
}

var (
	hostAppsCacheMu sync.RWMutex
	cachedHostApps  []HostAppItem
	cachedHostTime  time.Time

	startAppsMu      sync.RWMutex
	startAppsCached  []rawStartApp
	startAppsQueried bool
)

// InitHostAppsScan starts an asynchronous scan of the host environment at startup
// so that the first user request is answered immediately from cache.
func InitHostAppsScan() {
	go func() {
		_ = DetectHostAppsForce(true)
	}()
}

// DetectHostAppsForce scans host applications, optionally bypassing the in-memory cache.
func DetectHostAppsForce(force bool) []HostAppItem {
	if !force {
		hostAppsCacheMu.RLock()
		if len(cachedHostApps) > 0 && time.Since(cachedHostTime) < 3*time.Minute {
			res := make([]HostAppItem, len(cachedHostApps))
			copy(res, cachedHostApps)
			hostAppsCacheMu.RUnlock()
			return res
		}
		hostAppsCacheMu.RUnlock()
	}

	hostAppsCacheMu.Lock()
	defer hostAppsCacheMu.Unlock()

	if !force && len(cachedHostApps) > 0 && time.Since(cachedHostTime) < 3*time.Minute {
		res := make([]HostAppItem, len(cachedHostApps))
		copy(res, cachedHostApps)
		return res
	}

	results := runDetectHostApps()
	cachedHostApps = results
	cachedHostTime = time.Now()

	res := make([]HostAppItem, len(results))
	copy(res, results)
	return res
}

// DetectHostApps scans the host machine for installed applications, session tokens,
// and configurations across Linux, macOS, and Windows WSL without a fixed ceiling.
// It actively explores Windows Start Menu / StartApps, Linux .desktop files, and macOS apps.
// Only applications and sessions that were actually found on the host machine are returned.
// Results are cached in memory for sub-millisecond retrieval.
func DetectHostApps() []HostAppItem {
	return DetectHostAppsForce(false)
}

func runDetectHostApps() []HostAppItem {
	defs := getKnownAppDefs()
	items := make([]HostAppItem, 0, len(defs)+40)
	seen := make(map[string]bool)

	for _, def := range defs {
		paths := def.probeFn()
		detected := len(paths) > 0 || def.id == "host_bridge"
		if !detected {
			// User directive: show ONLY apps you found!
			continue
		}
		seen[strings.ToLower(def.name)] = true
		seen[strings.ToLower(def.id)] = true

		appType := "app_session"
		if def.id == "edge" || def.id == "chrome" || def.id == "firefox" {
			appType = "browser_session"
		} else if def.id == "youtube" {
			appType = "web_app"
		} else if def.id == "ssh" || def.id == "gh" || def.id == "docker" || def.id == "gcloud" || def.id == "azure" {
			appType = "credential"
		}

		items = append(items, HostAppItem{
			ID:              def.id,
			Name:            def.name,
			Category:        def.category,
			Description:     def.description,
			Detected:        true,
			DetectedPaths:   paths,
			DefaultSelected: false,
			AppType:         appType,
			Icon:            pickAppIcon(def.name),
		})
	}

	// Dynamically explore all other apps installed on Windows, Linux, or macOS
	dynamicApps := exploreDynamicApps(seen)
	for _, dapp := range dynamicApps {
		if dapp.Detected {
			items = append(items, dapp)
		}
	}

	// Filter strictly for detected items only
	detectedOnly := make([]HostAppItem, 0, len(items))
	for _, it := range items {
		if it.Detected {
			detectedOnly = append(detectedOnly, it)
		}
	}

	// Sort items alphabetically by Name
	sort.SliceStable(detectedOnly, func(i, j int) bool {
		return strings.ToLower(detectedOnly[i].Name) < strings.ToLower(detectedOnly[j].Name)
	})

	return detectedOnly
}

// MigrateHostApps packages selected host application sessions, tokens, and configurations,
// strips cache bloat to maintain zero redundant memory, streams them into the container's
// /home/headless volume, and installs the host execution bridge launchers.
func (m *Manager) MigrateHostApps(ctx context.Context, desktopID string, selectedIDs []string) error {
	if len(selectedIDs) == 0 {
		return nil
	}

	sanitizedID := sanitizeName(desktopID)
	containerName := "desktop_" + sanitizedID

	// Inspect container to ensure it exists
	inspect, err := m.client.ContainerInspect(ctx, containerName)
	if err != nil {
		return fmt.Errorf("inspect %s for migration: %w", containerName, err)
	}

	selectedMap := make(map[string]bool)
	for _, id := range selectedIDs {
		selectedMap[strings.ToLower(strings.TrimSpace(id))] = true
	}

	// Always include host_bridge when apps are being migrated
	selectedMap["host_bridge"] = true

	// Build tar archive buffer
	tarBuf := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuf)
	// Stream archive directly into container at /home/headless via pipe (zero RAM duplication)
	pr, pw := io.Pipe()
	copyOpts := container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: false,
		CopyUIDGID:                false,
	}

	copyErrCh := make(chan error, 1)
	go func() {
		defer pr.Close()
		copyErrCh <- m.client.CopyToContainer(ctx, inspect.ID, "/home/headless", pr, copyOpts)
	}()

	tw := tar.NewWriter(pw)

	defs := getKnownAppDefs()
	defMap := make(map[string]knownAppDef)
	for _, def := range defs {
		defMap[strings.ToLower(def.id)] = def
	}

	// Index all detected apps (including dynamic Windows and desktop apps)
	allDetected := DetectHostApps()
	appMap := make(map[string]HostAppItem)
	for _, app := range allDetected {
		appMap[strings.ToLower(app.ID)] = app
	}

	migratedCount := 0

	for id := range selectedMap {
		if def, ok := defMap[id]; ok {
			var paths []string
			if appItem, found := appMap[id]; found && len(appItem.DetectedPaths) > 0 {
				paths = appItem.DetectedPaths
			} else if def.probeFn != nil {
				paths = def.probeFn()
			}
			if len(paths) == 0 && def.id != "host_bridge" {
				continue
			}
			if err := def.packFn(tw, paths); err != nil {
				log.Printf("hostapps: packing %s for %s: %v", def.id, desktopID, err)
				continue
			}
			migratedCount++
		} else if app, ok := appMap[id]; ok {
			if err := packDynamicApp(tw, app); err != nil {
				log.Printf("hostapps: packing dynamic app %s for %s: %v", app.ID, desktopID, err)
				continue
			}
			migratedCount++
		}
	}

	if err := tw.Close(); err != nil {
		_ = pw.CloseWithError(err)
		<-copyErrCh
		return fmt.Errorf("close tar archive: %w", err)
	}
	_ = pw.Close()

	if tarBuf.Len() == 0 {
		return nil
	}

	// Stream archive directly into container at /home/headless
	copyOpts := container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: false,
		CopyUIDGID:                false,
	}

	if err := m.client.CopyToContainer(ctx, inspect.ID, "/home/headless", tarBuf, copyOpts); err != nil {
	if err := <-copyErrCh; err != nil {
		return fmt.Errorf("copy host apps to %s: %w", containerName, err)
	}

	log.Printf("hostapps: successfully migrated %d component(s) into %s", migratedCount, containerName)
	return nil
}

// -----------------------------------------------------------------------------
// Known Application Definitions & Packaging
// -----------------------------------------------------------------------------

// getHostHomeCandidates returns all candidate user home directories across native OS,
// WSL, and container mount paths (e.g. /host-home, /home/*).
func getHostHomeCandidates() []string {
	var homes []string
	seen := make(map[string]bool)

	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			seen[p] = true
			homes = append(homes, p)
		}
	}

	if h, err := os.UserHomeDir(); err == nil {
		add(h)
	}
	if envHome := os.Getenv("FLOWERA_HOST_HOME"); envHome != "" {
		add(envHome)
	}
	if envHostHome := os.Getenv("HOST_HOME"); envHostHome != "" {
		add(envHostHome)
	}

	for _, base := range []string{"/host-home", "/home"} {
		if entries, err := os.ReadDir(base); err == nil {
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					add(filepath.Join(base, e.Name()))
				}
			}
		}
	}
	return homes
}

// getWindowsCDriveCandidates returns all accessible root paths for Windows C: drive
// across native Windows, WSL mounts (/mnt/c), container mounts, and environment variables.
func getWindowsCDriveCandidates() []string {
	var drives []string
	seen := make(map[string]bool)

	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			seen[d] = true
			drives = append(drives, d)
		}
	}

	if d := os.Getenv("FLOWERA_HOST_MNT_C"); d != "" {
		add(d)
	}
	add("/mnt/c")
	add("/c")
	add("/mnt/host_c")
	if runtime.GOOS == "windows" {
		add("C:\\")
	}

	return drives
}

func getKnownAppDefs() []knownAppDef {
	homes := getHostHomeCandidates()
	cDrives := getWindowsCDriveCandidates()

	return []knownAppDef{
		{
			id:              "edge",
			name:            "Microsoft Edge",
			category:        "Browsers & Web",
			description:     "Profile, active cookies, sessions, login data (Cache stripped to save RAM)",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				// Linux Edge config across all host user home candidates
				for _, h := range homes {
					for _, c := range []string{
						filepath.Join(h, ".config", "microsoft-edge"),
						filepath.Join(h, "Library", "Application Support", "Microsoft Edge"),
					} {
						if fi, err := os.Stat(c); err == nil && fi.IsDir() {
							found = append(found, c)
						}
					}
				}
				// Windows WSL candidate paths
				for _, cDrive := range cDrives {
					pattern := filepath.Join(cDrive, "Users", "*", "AppData", "Local", "Microsoft", "Edge", "User Data")
					if matches, err := filepath.Glob(pattern); err == nil {
						found = append(found, matches...)
					}
					for _, edgeExe := range []string{
						filepath.Join(cDrive, "Program Files (x86)", "Microsoft", "Edge", "Application", "msedge.exe"),
						filepath.Join(cDrive, "Program Files", "Microsoft", "Edge", "Application", "msedge.exe"),
					} {
						if fi, err := os.Stat(edgeExe); err == nil && !fi.IsDir() {
							found = append(found, edgeExe)
							break
						}
					}
				}
				for _, app := range queryWindowsStartApps() {
					if strings.Contains(strings.ToLower(app.Name), "edge") {
						found = append(found, "windows_app:"+app.AppID)
						break
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						_ = writeDirToTar(tw, p, ".config/microsoft-edge", isCacheOrTransient)
						break
					}
				}
				edgeWrapper := `#!/usr/bin/env bash
if command -v cmd.exe >/dev/null 2>&1 || [ -e /mnt/c ]; then
    exec host-run cmd.exe /c start msedge "$@"
else
    exec host-run microsoft-edge "$@"
fi
`
				if err := writeStringToTar(tw, ".local/bin/edge-host", edgeWrapper, 0755); err != nil {
					return err
				}
				if iconSlug, svg := getAppSVGIcon("edge"); svg != "" {
					_ = writeStringToTar(tw, ".local/share/icons/hicolor/scalable/apps/"+iconSlug+".svg", svg, 0644)
				}
				edgeDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=Microsoft Edge (Host)
Comment=Run Microsoft Edge on host desktop with active login session
Exec=/home/headless/.local/bin/edge-host %U
Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/microsoft-edge.svg
Terminal=false
Categories=Network;WebBrowser;
`
				if err := writeStringToTar(tw, "Desktop/edge-host.desktop", edgeDesktop, 0755); err != nil {
					return err
				}
				return writeStringToTar(tw, ".local/share/applications/edge-host.desktop", edgeDesktop, 0755)
			},
		},
		{
			id:              "youtube",
			name:            "YouTube",
			category:        "Media & Web Apps",
			description:     "Launch YouTube on host desktop with active Google sign-in session",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, cDrive := range cDrives {
					pwaPattern := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "*", "*YouTube*.lnk")
					if matches, err := filepath.Glob(pwaPattern); err == nil && len(matches) > 0 {
						found = append(found, matches...)
					}
				}
				for _, app := range queryWindowsStartApps() {
					if strings.Contains(strings.ToLower(app.Name), "youtube") {
						found = append(found, "windows_app:"+app.AppID)
						break
					}
				}
				for _, h := range homes {
					linuxDesktopPattern := filepath.Join(h, ".local", "share", "applications", "*youtube*.desktop")
					if matches, err := filepath.Glob(linuxDesktopPattern); err == nil && len(matches) > 0 {
						found = append(found, matches...)
					}
				}
				if matches, err := filepath.Glob("/usr/share/applications/*youtube*.desktop"); err == nil && len(matches) > 0 {
					found = append(found, matches...)
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				youtubeWrapper := `#!/usr/bin/env bash
if command -v cmd.exe >/dev/null 2>&1 || [ -e /mnt/c ]; then
    exec host-run cmd.exe /c start msedge --app=https://www.youtube.com
else
    exec host-run host-open https://www.youtube.com
fi
`
				if err := writeStringToTar(tw, ".local/bin/youtube-host", youtubeWrapper, 0755); err != nil {
					return err
				}
				if iconSlug, svg := getAppSVGIcon("youtube"); svg != "" {
					_ = writeStringToTar(tw, ".local/share/icons/hicolor/scalable/apps/"+iconSlug+".svg", svg, 0644)
				}
				ytDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=YouTube (Host)
Comment=Watch YouTube on host desktop with active account session
Exec=/home/headless/.local/bin/youtube-host
Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/youtube.svg
Terminal=false
Categories=AudioVideo;Network;
`
				if err := writeStringToTar(tw, "Desktop/youtube-host.desktop", ytDesktop, 0755); err != nil {
					return err
				}
				return writeStringToTar(tw, ".local/share/applications/youtube-host.desktop", ytDesktop, 0755)
			},
		},
		{
			id:              "chrome",
			name:            "Google Chrome / Chromium",
			category:        "Browsers & Web",
			description:     "Profile, active cookies, sessions, login data (Cache stripped to save RAM)",
			defaultSelected: false,
			probeFn: func() []string {
				var candidates []string
				for _, h := range homes {
					candidates = append(candidates,
						filepath.Join(h, ".config", "google-chrome-for-testing"),
						filepath.Join(h, ".config", "google-chrome"),
						filepath.Join(h, ".config", "chromium"),
						filepath.Join(h, "Library", "Application Support", "Google", "Chrome"),
					)
				}
				// Windows WSL candidate paths
				for _, cDrive := range cDrives {
					pattern := filepath.Join(cDrive, "Users", "*", "AppData", "Local", "Google", "Chrome", "User Data")
					if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
						candidates = append(candidates, matches...)
					}
					chromeExe := filepath.Join(cDrive, "Program Files", "Google", "Chrome", "Application", "chrome.exe")
					if fi, err := os.Stat(chromeExe); err == nil && !fi.IsDir() {
						candidates = append(candidates, chromeExe)
					}
				}
				var found []string
				for _, p := range candidates {
					if _, err := os.Stat(p); err == nil {
						found = append(found, p)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						_ = writeDirToTar(tw, p, ".config/google-chrome", isCacheOrTransient)
						break
					}
				}
				header := &tar.Header{
					Name:     ".config/chromium",
					Linkname: "google-chrome",
					Typeflag: tar.TypeSymlink,
					Mode:     0755,
					ModTime:  time.Now(),
					Uid:      1000,
					Gid:      1000,
				}
				if err := tw.WriteHeader(header); err != nil {
					return err
				}
				chromeWrapper := "#!/usr/bin/env bash\nexec host-run google-chrome \"$@\"\n"
				if err := writeStringToTar(tw, ".local/bin/chrome-host", chromeWrapper, 0755); err != nil {
					return err
				}
				if iconSlug, svg := getAppSVGIcon("chrome"); svg != "" {
					_ = writeStringToTar(tw, ".local/share/icons/hicolor/scalable/apps/"+iconSlug+".svg", svg, 0644)
				}
				chromeDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=Google Chrome (Host)
Comment=Run Google Chrome on host desktop with zero duplicate VM RAM
Exec=/home/headless/.local/bin/chrome-host %U
Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/google-chrome.svg
Terminal=false
Categories=Network;WebBrowser;
`
				if err := writeStringToTar(tw, "Desktop/chrome-host.desktop", chromeDesktop, 0755); err != nil {
					return err
				}
				return writeStringToTar(tw, ".local/share/applications/chrome-host.desktop", chromeDesktop, 0755)
			},
		},
		{
			id:              "firefox",
			name:            "Mozilla Firefox",
			category:        "Browsers & Web",
			description:     "Profiles, saved logins, cookies, tabs (Cache stripped)",
			defaultSelected: false,
			probeFn: func() []string {
				var candidates []string
				for _, h := range homes {
					candidates = append(candidates,
						filepath.Join(h, ".mozilla", "firefox"),
						filepath.Join(h, "Library", "Application Support", "Firefox"),
					)
				}
				for _, cDrive := range cDrives {
					pattern := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "Mozilla", "Firefox", "Profiles")
					if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
						candidates = append(candidates, matches...)
					}
				}
				var found []string
				for _, p := range candidates {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						found = append(found, p)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".mozilla/firefox", isCacheOrTransient)
			},
		},
		{
			id:              "claude",
			name:            "Claude",
			category:        "Productivity",
			description:     "Claude desktop & AI assistant (renders directly in VM desktop with active session)",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, app := range queryWindowsStartApps() {
					if strings.Contains(strings.ToLower(app.Name), "claude") {
						found = append(found, "windows_app:"+app.AppID)
						break
					}
				}
				for _, h := range homes {
					for _, c := range []string{
						filepath.Join(h, ".cache", "claude"),
						filepath.Join(h, ".cache", "claude-cli-nodejs"),
						filepath.Join(h, ".config", "claude"),
					} {
						if _, err := os.Stat(c); err == nil {
							found = append(found, c)
						}
					}
				}
				for _, cDrive := range cDrives {
					pattern := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "Claude")
					if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
						found = append(found, matches...)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				claudeWrapper := `#!/usr/bin/env bash
if command -v chromium-browser >/dev/null 2>&1; then
    exec chromium-browser --app="https://claude.ai" "$@"
elif command -v google-chrome >/dev/null 2>&1; then
    exec google-chrome --app="https://claude.ai" "$@"
elif command -v chromium >/dev/null 2>&1; then
    exec chromium --app="https://claude.ai" "$@"
fi
exec host-run cmd.exe /c start https://claude.ai "$@"
`
				if err := writeStringToTar(tw, ".local/bin/claude-host", claudeWrapper, 0755); err != nil {
					return err
				}
				if iconSlug, svg := getAppSVGIcon("claude"); svg != "" {
					_ = writeStringToTar(tw, ".local/share/icons/hicolor/scalable/apps/"+iconSlug+".svg", svg, 0644)
				}
				claudeDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=Claude (Host)
Comment=Run Claude directly in VM desktop with zero host desktop popup
Exec=/home/headless/.local/bin/claude-host %U
Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/claude.svg
Terminal=false
Categories=Office;Utility;Network;
`
				if err := writeStringToTar(tw, "Desktop/claude-host.desktop", claudeDesktop, 0755); err != nil {
					return err
				}
				return writeStringToTar(tw, ".local/share/applications/claude-host.desktop", claudeDesktop, 0755)
			},
		},
		{
			id:              "chatgpt",
			name:            "ChatGPT",
			category:        "Productivity",
			description:     "ChatGPT desktop & AI assistant (renders directly in VM desktop with active session)",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, app := range queryWindowsStartApps() {
					lower := strings.ToLower(app.Name)
					if strings.Contains(lower, "chatgpt") || strings.Contains(lower, "chat gpt") || strings.Contains(strings.ToLower(app.AppID), "openai.codex") {
						found = append(found, "windows_app:"+app.AppID)
						break
					}
				}
				for _, cDrive := range cDrives {
					pattern := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "ChatGPT")
					if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
						found = append(found, matches...)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				chatgptWrapper := `#!/usr/bin/env bash
if command -v chromium-browser >/dev/null 2>&1; then
    exec chromium-browser --app="https://chatgpt.com" "$@"
elif command -v google-chrome >/dev/null 2>&1; then
    exec google-chrome --app="https://chatgpt.com" "$@"
elif command -v chromium >/dev/null 2>&1; then
    exec chromium --app="https://chatgpt.com" "$@"
fi
exec host-run cmd.exe /c start https://chatgpt.com "$@"
`
				if err := writeStringToTar(tw, ".local/bin/chatgpt-host", chatgptWrapper, 0755); err != nil {
					return err
				}
				if iconSlug, svg := getAppSVGIcon("chatgpt"); svg != "" {
					_ = writeStringToTar(tw, ".local/share/icons/hicolor/scalable/apps/"+iconSlug+".svg", svg, 0644)
				}
				chatgptDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=ChatGPT (Host)
Comment=Run ChatGPT directly in VM desktop with zero host desktop popup
Exec=/home/headless/.local/bin/chatgpt-host %U
Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/chatgpt.svg
Terminal=false
Categories=Office;Utility;Network;
`
				if err := writeStringToTar(tw, "Desktop/chatgpt-host.desktop", chatgptDesktop, 0755); err != nil {
					return err
				}
				return writeStringToTar(tw, ".local/share/applications/chatgpt-host.desktop", chatgptDesktop, 0755)
			},
		},
		{
			id:              "gh",
			name:            "GitHub CLI & Git Config",
			category:        "Development",
			description:     "Logged-in GitHub token, credentials, and ~/.gitconfig",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, h := range homes {
					ghConfig := filepath.Join(h, ".config", "gh")
					if fi, err := os.Stat(ghConfig); err == nil && fi.IsDir() {
						found = append(found, ghConfig)
					}
					gitConfig := filepath.Join(h, ".gitconfig")
					if fi, err := os.Stat(gitConfig); err == nil && !fi.IsDir() {
						found = append(found, gitConfig)
					}
				}
				for _, cDrive := range cDrives {
					winGH := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "GitHub CLI")
					if matches, err := filepath.Glob(winGH); err == nil {
						found = append(found, matches...)
					}
					winGit := filepath.Join(cDrive, "Users", "*", ".gitconfig")
					if matches, err := filepath.Glob(winGit); err == nil {
						found = append(found, matches...)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					if strings.HasSuffix(p, ".gitconfig") {
						if err := writeFileToTar(tw, p, ".gitconfig", 0644); err != nil {
							return err
						}
					} else {
						if err := writeDirToTar(tw, p, ".config/gh", nil); err != nil {
							return err
						}
					}
				}
				return nil
			},
		},
		{
			id:              "docker",
			name:            "Docker Hub & Engine Config",
			category:        "Development",
			description:     "Docker credentials, contexts, and client config",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, h := range homes {
					cfg := filepath.Join(h, ".docker")
					if fi, err := os.Stat(cfg); err == nil && fi.IsDir() {
						found = append(found, cfg)
					}
				}
				for _, cDrive := range cDrives {
					winCfg := filepath.Join(cDrive, "Users", "*", ".docker")
					if matches, err := filepath.Glob(winCfg); err == nil {
						found = append(found, matches...)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".docker", isCacheOrTransient)
			},
		},
		{
			id:              "vscode",
			name:            "VS Code / OpenCode Settings",
			category:        "Development",
			description:     "Extensions, keybindings, settings, workspace storage",
			defaultSelected: false,
			probeFn: func() []string {
				var candidates []string
				for _, h := range homes {
					candidates = append(candidates,
						filepath.Join(h, ".config", "Code", "User"),
						filepath.Join(h, ".vscode", "extensions"),
						filepath.Join(h, ".opencode"),
						filepath.Join(h, "Library", "Application Support", "Code", "User"),
					)
				}
				for _, cDrive := range cDrives {
					winCode := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "Code", "User")
					if matches, err := filepath.Glob(winCode); err == nil {
						candidates = append(candidates, matches...)
					}
					winExt := filepath.Join(cDrive, "Users", "*", ".vscode", "extensions")
					if matches, err := filepath.Glob(winExt); err == nil {
						candidates = append(candidates, matches...)
					}
				}
				var found []string
				for _, p := range candidates {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						found = append(found, p)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					dest := ".config/Code/User"
					if strings.Contains(p, ".vscode") {
						dest = ".vscode/extensions"
					} else if strings.Contains(p, ".opencode") {
						dest = ".opencode"
					}
					if err := writeDirToTar(tw, p, dest, isCacheOrTransient); err != nil {
						return err
					}
				}
				codeWrapper := "#!/usr/bin/env bash\nexec host-run code \"$@\"\n"
				if err := writeStringToTar(tw, ".local/bin/code-host", codeWrapper, 0755); err != nil {
					return err
				}
				if iconSlug, svg := getAppSVGIcon("vscode"); svg != "" {
					_ = writeStringToTar(tw, ".local/share/icons/hicolor/scalable/apps/"+iconSlug+".svg", svg, 0644)
				}
				codeDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=VS Code (Host)
Comment=Run Visual Studio Code on host desktop with zero duplicate VM RAM
Exec=/home/headless/.local/bin/code-host %F
Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/vscode.svg
Terminal=false
Categories=Development;IDE;
`
				if err := writeStringToTar(tw, "Desktop/code-host.desktop", codeDesktop, 0755); err != nil {
					return err
				}
				return writeStringToTar(tw, ".local/share/applications/code-host.desktop", codeDesktop, 0755)
			},
		},
		{
			id:              "ssh",
			name:            "SSH Keys & Known Hosts",
			category:        "Development",
			description:     "Private keys, public keys, config (strictly chmod 0600)",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, h := range homes {
					sshDir := filepath.Join(h, ".ssh")
					if fi, err := os.Stat(sshDir); err == nil && fi.IsDir() {
						found = append(found, sshDir)
					}
				}
				for _, cDrive := range cDrives {
					winSSH := filepath.Join(cDrive, "Users", "*", ".ssh")
					if matches, err := filepath.Glob(winSSH); err == nil {
						found = append(found, matches...)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".ssh", func(name, fullPath string, isDir bool) bool {
					lower := strings.ToLower(name)
					return strings.HasSuffix(lower, ".old") || strings.HasSuffix(lower, ".bak")
				})
			},
		},
		{
			id:              "gcloud",
			name:            "Google Cloud CLI (gcloud)",
			category:        "Development",
			description:     "Active GCP credentials, default project, configurations",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, h := range homes {
					cfg := filepath.Join(h, ".config", "gcloud")
					if fi, err := os.Stat(cfg); err == nil && fi.IsDir() {
						found = append(found, cfg)
					}
				}
				for _, cDrive := range cDrives {
					winCfg := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "gcloud")
					if matches, err := filepath.Glob(winCfg); err == nil {
						found = append(found, matches...)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".config/gcloud", isCacheOrTransient)
			},
		},
		{
			id:              "azure",
			name:            "Azure CLI (az)",
			category:        "Development",
			description:     "Active Azure login tokens, subscriptions, profile",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, h := range homes {
					cfg := filepath.Join(h, ".azure")
					if fi, err := os.Stat(cfg); err == nil && fi.IsDir() {
						found = append(found, cfg)
					}
				}
				for _, cDrive := range cDrives {
					winCfg := filepath.Join(cDrive, "Users", "*", ".azure")
					if matches, err := filepath.Glob(winCfg); err == nil {
						found = append(found, matches...)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".azure", isCacheOrTransient)
			},
		},
		{
			id:              "keyrings",
			name:            "System Keyrings & PKI",
			category:        "Development",
			description:     "Secret Service keyrings and local certificates",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, h := range homes {
					for _, p := range []string{
						filepath.Join(h, ".local", "share", "keyrings"),
						filepath.Join(h, ".pki"),
					} {
						if fi, err := os.Stat(p); err == nil && fi.IsDir() {
							found = append(found, p)
						}
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					dest := ".local/share/keyrings"
					if strings.Contains(p, ".pki") {
						dest = ".pki"
					}
					if err := writeDirToTar(tw, p, dest, nil); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			id:              "dotfiles",
			name:            "Shell Profiles & Dotfiles",
			category:        "Development",
			description:     "~/.bashrc, ~/.profile, ~/.zshrc shell environment",
			defaultSelected: false,
			probeFn: func() []string {
				var found []string
				for _, h := range homes {
					for _, f := range []string{".bashrc", ".profile", ".zshrc", ".bash_aliases"} {
						p := filepath.Join(h, f)
						if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
							found = append(found, p)
						}
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					name := filepath.Base(p)
					if err := writeFileToTar(tw, p, name, 0644); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			id:              "host_bridge",
			name:            "Host Execution Bridge (Zero-Memory Launcher)",
			category:        "Host Bridge",
			description:     "Light emulation scripts and desktop launchers to execute apps natively on host",
			defaultSelected: false,
			probeFn: func() []string {
				return []string{"host_bridge"}
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				return packHostBridgeLaunchers(tw)
			},
		},
	}
}

// -----------------------------------------------------------------------------
// Dynamic Discovery Providers (Windows/WSL, Linux, macOS)
// -----------------------------------------------------------------------------

// IsWSLOrWindows returns true if running directly on Windows or inside a WSL environment.
func IsWSLOrWindows() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	if os.Getenv("WSLENV") != "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	for _, drive := range getWindowsCDriveCandidates() {
		if fi, err := os.Stat(drive); err == nil && fi.IsDir() {
			return true
		}
	}
	if data, err := os.ReadFile("/proc/version"); err == nil {
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl") {
			return true
		}
	}
	return false
}

type rawStartApp struct {
	Name  string `json:"Name"`
	AppID string `json:"AppID"`
}

func parseGetStartAppsJSON(data []byte) []rawStartApp {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	var list []rawStartApp
	if err := json.Unmarshal(data, &list); err == nil {
		return list
	}
	var single rawStartApp
	if err := json.Unmarshal(data, &single); err == nil && single.Name != "" {
		return []rawStartApp{single}
	}
	return nil
}

func findPowershellExe() string {
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	for _, drive := range getWindowsCDriveCandidates() {
		for _, winDir := range []string{"WINDOWS", "Windows", "windows"} {
			for _, sysDir := range []string{"System32", "system32"} {
				p := filepath.Join(drive, winDir, sysDir, "WindowsPowerShell", "v1.0", "powershell.exe")
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}
	return ""
}

func findDockerBinary() string {
	candidates := []string{"docker", "/usr/bin/docker", "/usr/local/bin/docker", "/bin/docker"}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && (fi.Mode()&0111 != 0) {
			return c
		}
	}
	return ""
}

func queryWindowsStartApps() []rawStartApp {
	startAppsMu.RLock()
	if startAppsQueried {
		cached := startAppsCached
		startAppsMu.RUnlock()
		return cached
	}
	startAppsMu.RUnlock()

	startAppsMu.Lock()
	defer startAppsMu.Unlock()
	if startAppsQueried {
		return startAppsCached
	}

	// 1. Check pre-cached file generated by rebuild.sh / build.sh
	for _, cachePath := range []string{"/tmp/flowera_host_apps.json", "/data/flowera_host_apps.json"} {
		if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 {
			if apps := parseGetStartAppsJSON(data); len(apps) > 0 {
				startAppsCached = apps
				startAppsQueried = true
				return apps
			}
		}
	}

	// 2. Direct execution of powershell.exe with -InputFormat None and empty stdin
	psExe := findPowershellExe()
	if psExe != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, psExe, "-NoProfile", "-NonInteractive", "-InputFormat", "None", "-Command", "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress")
		cmd.Stdin = strings.NewReader("")
		if output, err := cmd.Output(); err == nil && len(output) > 0 {
			if apps := parseGetStartAppsJSON(output); len(apps) > 0 {
				startAppsCached = apps
				startAppsQueried = true
				return apps
			}
		}
	}

	// 3. Containerized host execution via Docker socket if running inside container
	// Only attempt if Windows C drive candidate exists (avoid 10s nsenter timeout on pure Linux)
	hasWindowsDrive := len(getWindowsCDriveCandidates()) > 0 || psExe != ""
	if hasWindowsDrive {
		if _, err := os.Stat("/var/run/docker.sock"); err == nil {
			if dockerBin := findDockerBinary(); dockerBin != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				psCmd := "PATH=$PATH:/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0:/mnt/c/Windows/System32/WindowsPowerShell/v1.0:/mnt/c/WINDOWS/system32:/mnt/c/Windows/system32 powershell.exe -NoProfile -NonInteractive -InputFormat None -Command 'Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress' < /dev/null"
				cmd := exec.CommandContext(ctx, dockerBin, "run", "--rm", "--privileged", "--pid=host", "alpine", "nsenter", "-t", "1", "-m", "-u", "-n", "-i", "bash", "-c", psCmd)
				cmd.Stdin = strings.NewReader("")
				if output, err := cmd.Output(); err == nil && len(output) > 0 {
					if apps := parseGetStartAppsJSON(output); len(apps) > 0 {
						startAppsCached = apps
						startAppsQueried = true
						return apps
					}
				}
			}
		}
	}

	startAppsQueried = true
	return nil
}

func wslToWindowsPath(wslPath string) string {
	if strings.HasPrefix(wslPath, "/mnt/") && len(wslPath) > 6 && wslPath[6] == '/' {
		drive := strings.ToUpper(string(wslPath[5]))
		rest := strings.ReplaceAll(wslPath[6:], "/", "\\")
		return drive + ":" + rest
	}
	if strings.HasPrefix(wslPath, "/") && len(wslPath) > 2 && wslPath[2] == '/' {
		drive := strings.ToUpper(string(wslPath[1]))
		rest := strings.ReplaceAll(wslPath[2:], "/", "\\")
		return drive + ":" + rest
	}
	return strings.ReplaceAll(wslPath, "/", "\\")
}

func cleanAppShortcutName(raw string) string {
	name := strings.TrimSpace(raw)
	name = regexp.MustCompile(`\s*\((?:64|32)-bit\)`).ReplaceAllString(name, "")
	name = regexp.MustCompile(`\s*-\s*Shortcut$`).ReplaceAllString(name, "")
	return strings.TrimSpace(name)
}

func isIgnoredWindowsApp(name string) bool {
	lower := strings.ToLower(name)
	ignoreKeywords := []string{
		"uninstall", "remove", "help", "readme", "documentation",
		"license", "manual", "release notes", "configure", "setup wizard",
		"website", "check for updates", "about",
	}
	for _, kw := range ignoreKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func categorizeAppName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "chrome") || strings.Contains(lower, "edge") ||
		strings.Contains(lower, "firefox") || strings.Contains(lower, "brave") ||
		strings.Contains(lower, "opera") || strings.Contains(lower, "browser") ||
		strings.Contains(lower, "vivaldi") || strings.Contains(lower, "arc"):
		return "Browsers & Web"
	case strings.Contains(lower, "youtube") || strings.Contains(lower, "spotify") ||
		strings.Contains(lower, "music") || strings.Contains(lower, "video") ||
		strings.Contains(lower, "vlc") || strings.Contains(lower, "media") ||
		strings.Contains(lower, "netflix") || strings.Contains(lower, "twitch"):
		return "Media & Entertainment"
	case strings.Contains(lower, "code") || strings.Contains(lower, "visual studio") ||
		strings.Contains(lower, "git") || strings.Contains(lower, "terminal") ||
		strings.Contains(lower, "powershell") || strings.Contains(lower, "bash") ||
		strings.Contains(lower, "sublime") || strings.Contains(lower, "intellij") ||
		strings.Contains(lower, "pycharm") || strings.Contains(lower, "docker"):
		return "Development"
	case strings.Contains(lower, "discord") || strings.Contains(lower, "slack") ||
		strings.Contains(lower, "teams") || strings.Contains(lower, "telegram") ||
		strings.Contains(lower, "whatsapp") || strings.Contains(lower, "zoom"):
		return "Communication"
	case strings.Contains(lower, "word") || strings.Contains(lower, "excel") ||
		strings.Contains(lower, "powerpoint") || strings.Contains(lower, "office") ||
		strings.Contains(lower, "notion") || strings.Contains(lower, "obsidian") ||
		strings.Contains(lower, "notes") || strings.Contains(lower, "calc"):
		return "Productivity"
	default:
		return "Desktop Apps"
	}
}

func isPopularApp(name string) bool {
	lower := strings.ToLower(name)
	popular := []string{
		"edge", "chrome", "firefox", "brave", "youtube",
		"spotify", "discord", "slack", "visual studio code", "code",
	}
	for _, p := range popular {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// getWebAppURLForApp detects if an application name or host command/AppID represents
// an AI service, PWA, or cloud application that should render directly in the VM
// desktop using Chromium standalone app mode instead of popping up on the host physical monitor.
func getWebAppURLForApp(name, cmdOrId string) string {
	lowerCmd := strings.ToLower(cmdOrId)
	lowerName := strings.ToLower(name)

	// Direct URL shortcuts
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(cmdOrId, prefix) {
			return cmdOrId
		}
		if idx := strings.Index(cmdOrId, prefix); idx != -1 {
			fields := strings.Fields(cmdOrId[idx:])
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}

	// Known AI, cloud, and productivity applications
	switch {
	case strings.Contains(lowerName, "claude") || strings.Contains(lowerCmd, "claude"):
		return "https://claude.ai"
	case strings.Contains(lowerName, "chatgpt") || strings.Contains(lowerName, "chat gpt") ||
		strings.Contains(lowerCmd, "openai.codex") || strings.Contains(lowerCmd, "chatgpt"):
		return "https://chatgpt.com"
	case strings.Contains(lowerName, "copilot"):
		return "https://copilot.microsoft.com"
	case strings.Contains(lowerName, "perplexity"):
		return "https://www.perplexity.ai"
	case strings.Contains(lowerName, "youtube") || strings.Contains(lowerCmd, "youtube"):
		return "https://www.youtube.com"
	case strings.Contains(lowerName, "whatsapp") || strings.Contains(lowerCmd, "whatsapp"):
		return "https://web.whatsapp.com"
	case strings.Contains(lowerName, "discord"):
		return "https://discord.com/app"
	case strings.Contains(lowerName, "slack"):
		return "https://app.slack.com"
	case strings.Contains(lowerName, "spotify"):
		return "https://open.spotify.com"
	case strings.Contains(lowerName, "notion"):
		return "https://www.notion.so"
	case strings.Contains(lowerName, "figma"):
		return "https://www.figma.com"
	case strings.Contains(lowerName, "linear"):
		return "https://linear.app"
	case strings.Contains(lowerName, "github"):
		return "https://github.com"
	case strings.Contains(lowerName, "telegram"):
		return "https://web.telegram.org"
	case strings.Contains(lowerName, "reddit"):
		return "https://www.reddit.com"
	case strings.Contains(lowerName, "gmail") || strings.Contains(lowerName, "google mail"):
		return "https://mail.google.com"
	case strings.Contains(lowerName, "google drive"):
		return "https://drive.google.com"
	case strings.Contains(lowerName, "google docs"):
		return "https://docs.google.com"
	case strings.Contains(lowerName, "google sheets"):
		return "https://sheets.google.com"
	case strings.Contains(lowerName, "google meet"):
		return "https://meet.google.com"
	default:
		return ""
	}
}

func pickAppIcon(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic"):
		return "help-browser"
	case strings.Contains(lower, "chatgpt") || strings.Contains(lower, "openai"):
		return "user-available"
	case strings.Contains(lower, "copilot") || strings.Contains(lower, "perplexity"):
		return "system-search"
	case strings.Contains(lower, "edge"):
		return "microsoft-edge"
	case strings.Contains(lower, "chrome"):
		return "google-chrome"
	case strings.Contains(lower, "firefox"):
		return "firefox"
	case strings.Contains(lower, "browser"):
		return "web-browser"
	case strings.Contains(lower, "youtube"):
		return "video-display"
	case strings.Contains(lower, "spotify") || strings.Contains(lower, "music"):
		return "audio-x-generic"
	case strings.Contains(lower, "code") || strings.Contains(lower, "studio"):
		return "code"
	case strings.Contains(lower, "terminal") || strings.Contains(lower, "powershell"):
		return "utilities-terminal"
	case strings.Contains(lower, "discord") || strings.Contains(lower, "slack") || strings.Contains(lower, "chat"):
		return "user-available"
	case strings.Contains(lower, "calc"):
		return "accessories-calculator"
	case strings.Contains(lower, "note") || strings.Contains(lower, "text"):
		return "text-editor"
	default:
		return "application-x-executable"
	}
}

func sanitizeAppSlug(name string) string {
	slug := strings.ToLower(name)
	slug = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(slug, "-")
	return strings.Trim(slug, "-")
}

func getCategoriesForApp(cat string) string {
	switch cat {
	case "Browsers & Web":
		return "Network;WebBrowser;"
	case "Media & Entertainment":
		return "AudioVideo;Audio;Video;"
	case "Development":
		return "Development;IDE;Utility;"
	case "Communication":
		return "Network;Chat;InstantMessaging;"
	case "Productivity":
		return "Office;Utility;"
	default:
		return "Utility;Application;"
	}
}

func scanWindowsStartMenu(seen map[string]bool) []HostAppItem {
	var items []HostAppItem

	var roots []string
	for _, drive := range getWindowsCDriveCandidates() {
		// ProgramData start menu
		for _, progData := range []string{
			filepath.Join(drive, "ProgramData", "Microsoft", "Windows", "Start Menu", "Programs"),
			filepath.Join(drive, "ProgramData", "Microsoft", "WINDOWS", "Start Menu", "Programs"),
		} {
			if fi, err := os.Stat(progData); err == nil && fi.IsDir() {
				roots = append(roots, progData)
			}
		}

		// User Roaming start menu
		for _, userPattern := range []string{
			filepath.Join(drive, "Users", "*", "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs"),
			filepath.Join(drive, "Users", "*", "AppData", "Roaming", "Microsoft", "WINDOWS", "Start Menu", "Programs"),
		} {
			if matches, err := filepath.Glob(userPattern); err == nil {
				roots = append(roots, matches...)
			}
		}

		// User Local Programs (e.g. Cursor, VS Code, Bruno, agent-orchestrator)
		localProgPattern := filepath.Join(drive, "Users", "*", "AppData", "Local", "Programs")
		if matches, err := filepath.Glob(localProgPattern); err == nil {
			roots = append(roots, matches...)
		}

		// User Desktop shortcuts
		desktopPattern := filepath.Join(drive, "Users", "*", "Desktop")
		if matches, err := filepath.Glob(desktopPattern); err == nil {
			roots = append(roots, matches...)
		}
	}

	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			if info.IsDir() {
				rel, err := filepath.Rel(root, path)
				if err != nil || rel == "." {
					return nil
				}
				// Limit recursion depth to 2 levels maximum (e.g. Programs / App / App.exe or Programs / Category / App.lnk)
				if strings.Count(rel, string(filepath.Separator)) >= 2 {
					return filepath.SkipDir
				}
				nameLower := strings.ToLower(info.Name())
				if nameLower == "node_modules" || nameLower == "resources" || nameLower == "locales" ||
					nameLower == "cache" || nameLower == ".git" || nameLower == "logs" || nameLower == "temp" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".lnk" && ext != ".exe" {
				return nil
			}
			if ext == ".exe" {
				base := strings.ToLower(filepath.Base(path))
				if strings.Contains(base, "unins") || strings.Contains(base, "update") || strings.Contains(base, "crash") || strings.Contains(base, "helper") {
					return nil
				}
			}

			rawName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			cleanName := cleanAppShortcutName(rawName)
			if cleanName == "" || isIgnoredWindowsApp(cleanName) {
				return nil
			}

			normKey := strings.ToLower(cleanName)
			if seen[normKey] {
				return nil
			}
			seen[normKey] = true

			winPath := wslToWindowsPath(path)
			hostCmd := fmt.Sprintf(`cmd.exe /c start "" "%s"`, winPath)

			id := "win_" + sanitizeAppSlug(cleanName)
			if seen[id] {
				return nil
			}
			seen[id] = true

			items = append(items, HostAppItem{
				ID:              id,
				Name:            cleanName,
				Category:        categorizeAppName(cleanName),
				Description:     "Windows desktop application (runs on host desktop)",
				Detected:        true,
				DetectedPaths:   []string{path},
				DefaultSelected: false,
				AppType:         "windows_app",
				HostCmd:         hostCmd,
				Icon:            pickAppIcon(cleanName),
			})
			return nil
		})
	}

	return items
}

type desktopMeta struct {
	Name      string
	Exec      string
	Icon      string
	Comment   string
	NoDisplay bool
}

func parseDesktopFile(path string) desktopMeta {
	f, err := os.Open(path)
	if err != nil {
		return desktopMeta{}
	}
	defer f.Close()

	var meta desktopMeta
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Name=") && meta.Name == "" {
			meta.Name = strings.TrimPrefix(line, "Name=")
		} else if strings.HasPrefix(line, "Exec=") && meta.Exec == "" {
			meta.Exec = strings.TrimPrefix(line, "Exec=")
		} else if strings.HasPrefix(line, "Icon=") && meta.Icon == "" {
			meta.Icon = strings.TrimPrefix(line, "Icon=")
		} else if strings.HasPrefix(line, "Comment=") && meta.Comment == "" {
			meta.Comment = strings.TrimPrefix(line, "Comment=")
		} else if strings.HasPrefix(line, "NoDisplay=true") {
			meta.NoDisplay = true
		}
	}
	return meta
}

func scanLinuxDesktopFiles(seen map[string]bool) []HostAppItem {
	if runtime.GOOS != "linux" {
		return nil
	}

	dirs := []string{
		"/usr/share/applications",
		"/usr/local/share/applications",
	}
	for _, h := range getHostHomeCandidates() {
		dirs = append(dirs, filepath.Join(h, ".local", "share", "applications"))
	}

	var items []HostAppItem

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".desktop") {
				continue
			}

			// Don't re-scan flowera host bridge desktop files
			if strings.HasSuffix(e.Name(), "-host.desktop") {
				continue
			}

			fullPath := filepath.Join(dir, e.Name())
			meta := parseDesktopFile(fullPath)
			if meta.Name == "" || meta.NoDisplay {
				continue
			}

			normKey := strings.ToLower(meta.Name)
			if seen[normKey] {
				continue
			}
			seen[normKey] = true

			id := "linux_" + sanitizeAppSlug(meta.Name)
			if seen[id] {
				continue
			}
			seen[id] = true

			cleanExec := strings.Split(meta.Exec, " %")[0]
			hostCmd := fmt.Sprintf("/usr/bin/env %s", cleanExec)

			items = append(items, HostAppItem{
				ID:              id,
				Name:            meta.Name,
				Category:        categorizeAppName(meta.Name),
				Description:     meta.Comment,
				Detected:        true,
				DetectedPaths:   []string{fullPath},
				DefaultSelected: false,
				AppType:         "desktop_app",
				HostCmd:         hostCmd,
				Icon:            meta.Icon,
			})
		}
	}

	return items
}

func scanMacAppBundles(seen map[string]bool) []HostAppItem {
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, _ := os.UserHomeDir()
	dirs := []string{"/Applications", filepath.Join(home, "Applications")}

	var items []HostAppItem
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasSuffix(e.Name(), ".app") {
				continue
			}
			appName := strings.TrimSuffix(e.Name(), ".app")
			normKey := strings.ToLower(appName)
			if seen[normKey] {
				continue
			}
			seen[normKey] = true

			fullPath := filepath.Join(dir, e.Name())
			id := "mac_" + sanitizeAppSlug(appName)
			if seen[id] {
				continue
			}
			seen[id] = true

			items = append(items, HostAppItem{
				ID:              id,
				Name:            appName,
				Category:        categorizeAppName(appName),
				Description:     "macOS application bundle",
				Detected:        true,
				DetectedPaths:   []string{fullPath},
				DefaultSelected: false,
				AppType:         "desktop_app",
				HostCmd:         fmt.Sprintf(`open -a "%s"`, fullPath),
				Icon:            pickAppIcon(appName),
			})
		}
	}
	return items
}

func exploreDynamicApps(seen map[string]bool) []HostAppItem {
	var items []HostAppItem

	// 1. If in WSL or Windows, explore Windows StartApps and Start Menu shortcuts
	if IsWSLOrWindows() {
		// PowerShell StartApps (Win32, UWP, Chrome/Edge PWAs)
		startApps := queryWindowsStartApps()
		for _, app := range startApps {
			if app.Name == "" || isIgnoredWindowsApp(app.Name) {
				continue
			}
			normKey := strings.ToLower(app.Name)
			if seen[normKey] {
				continue
			}
			seen[normKey] = true

			id := "win_" + sanitizeAppSlug(app.Name)
			if seen[id] {
				continue
			}
			seen[id] = true

			hostCmd := fmt.Sprintf(`cmd.exe /c start explorer.exe shell:AppsFolder\%s`, app.AppID)
			desc := "Windows application (runs on host desktop)"
			if getWebAppURLForApp(app.Name, app.AppID) != "" {
				desc = "AI & cloud application (renders directly in VM desktop)"
			}
			items = append(items, HostAppItem{
				ID:              id,
				Name:            app.Name,
				Category:        categorizeAppName(app.Name),
				Description:     desc,
				Detected:        true,
				DefaultSelected: false,
				AppType:         "windows_app",
				HostCmd:         hostCmd,
				Icon:            pickAppIcon(app.Name),
			})
		}

		// Windows Start Menu (.lnk shortcuts including Chrome Apps & Edge Apps)
		menuApps := scanWindowsStartMenu(seen)
		items = append(items, menuApps...)
	}

	// 2. Linux desktop applications
	linuxApps := scanLinuxDesktopFiles(seen)
	items = append(items, linuxApps...)

	// 3. macOS applications
	macApps := scanMacAppBundles(seen)
	items = append(items, macApps...)

	return items
}

func packDynamicApp(tw *tar.Writer, app HostAppItem) error {
	if len(app.DetectedPaths) > 0 && app.TargetConfigDir != "" {
		for _, p := range app.DetectedPaths {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				_ = writeDirToTar(tw, p, app.TargetConfigDir, isCacheOrTransient)
				break
			}
		}
	}

	if app.HostCmd != "" {
		slug := sanitizeAppSlug(app.Name)
		if slug == "" {
			slug = app.ID
		}
		icon := app.Icon

		// Check if we have an embedded SVG icon for this app
		iconSlug, svgContent := getAppSVGIcon(app.Name)
		if svgContent == "" && icon != "" {
			iconSlug, svgContent = getAppSVGIcon(icon)
		}

		if svgContent != "" {
			iconTarget := fmt.Sprintf(".local/share/icons/hicolor/scalable/apps/%s.svg", iconSlug)
			_ = writeStringToTar(tw, iconTarget, svgContent, 0644)
			icon = fmt.Sprintf("/home/headless/%s", iconTarget)
		} else if icon != "" && filepath.IsAbs(icon) {
			// If app has an absolute host icon path that exists, copy it into the archive
			if fi, err := os.Stat(icon); err == nil && !fi.IsDir() {
				ext := filepath.Ext(icon)
				if ext == "" {
					ext = ".png"
				}
				iconTarget := fmt.Sprintf(".local/share/icons/hicolor/48x48/apps/%s%s", slug, ext)
				if err := writeFileToTar(tw, icon, iconTarget, 0644); err == nil {
					icon = fmt.Sprintf("/home/headless/%s", iconTarget)
				}
			}
		}

		if icon == "" {
			icon = pickAppIcon(app.Name)
		}
		cat := getCategoriesForApp(app.Category)

		var wrapperScript string
		webURL := getWebAppURLForApp(app.Name, app.HostCmd)
		if webURL != "" {
			wrapperScript = fmt.Sprintf(`#!/usr/bin/env bash
# First preference: Render directly inside the VM desktop using Chromium standalone app mode
if command -v chromium-browser >/dev/null 2>&1; then
    exec chromium-browser --app=%q "$@"
elif command -v google-chrome >/dev/null 2>&1; then
    exec google-chrome --app=%q "$@"
elif command -v chromium >/dev/null 2>&1; then
    exec chromium --app=%q "$@"
fi
exec host-run %s "$@"
`, webURL, webURL, webURL, app.HostCmd)
		} else {
			wrapperScript = fmt.Sprintf("#!/usr/bin/env bash\nexec host-run %s \"$@\"\n", app.HostCmd)
		}

		if err := writeStringToTar(tw, ".local/bin/"+slug+"-host", wrapperScript, 0755); err != nil {
			return err
		}

		launcherContent := fmt.Sprintf(`[Desktop Entry]
Version=1.0
Type=Application
Name=%s (Host)
Comment=%s
Exec=/home/headless/.local/bin/%s-host %%U
Icon=%s
Terminal=false
Categories=%s
`, app.Name, app.Description, slug, icon, cat)

		if err := writeStringToTar(tw, "Desktop/"+slug+"-host.desktop", launcherContent, 0755); err != nil {
			return err
		}
		if err := writeStringToTar(tw, ".local/share/applications/"+slug+"-host.desktop", launcherContent, 0755); err != nil {
			return err
		}
	}

	return nil
}

// -----------------------------------------------------------------------------
// Host Execution Bridge Packaging & Scripts
// -----------------------------------------------------------------------------

func packHostBridgeLaunchers(tw *tar.Writer) error {
	hostRunScript := `#!/usr/bin/env bash
# host-run: Dispatches command execution directly to the host desktop machine.
set -e

CP_URL="${CONTROL_PLANE_URL:-${FLOWERA_CONTROL_PLANE:-http://host.docker.internal:8080}}"
TUNNEL_PORT="${FLOWERA_HOST_PORT:-2222}"

usage() {
    cat << EOF
Usage: host-run <command> [args...]
       host-run open <file-or-url>
       host-run clip <text or pipe from stdin>

Runs apps directly on the host machine with zero duplicate VM RAM.
EOF
    exit 0
}

[ $# -eq 0 ] && usage

subcmd="$1"

case "$subcmd" in
    open)
        shift
        target="${1:-.}"
        if curl -s -f -X POST "${CP_URL}/api/host-bridge/open" -H "Content-Type: application/json" -d "{\"target\": \"${target}\"}" >/dev/null 2>&1; then
            echo "Opened on host desktop: ${target}"
            exit 0
        fi
        if timeout 1 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/${TUNNEL_PORT}" 2>/dev/null; then
            ssh -F /dev/null -p "${TUNNEL_PORT}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null 127.0.0.1 "xdg-open '${target}' 2>/dev/null || open '${target}' 2>/dev/null || cmd.exe /c start '' '${target}'" 2>/dev/null || true
            exit 0
        fi
        echo "Error: Host bridge not reachable at ${CP_URL} or tunnel port ${TUNNEL_PORT}."
        exit 1
        ;;
    clip)
        shift
        if [ $# -gt 0 ]; then
            text="$*"
        else
            text=$(cat)
        fi
        if curl -s -f -X POST "${CP_URL}/api/host-bridge/clip" -H "Content-Type: application/json" -d "{\"text\": $(printf '%s' "$text" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')}" >/dev/null 2>&1; then
            echo "Copied to host clipboard!"
            exit 0
        fi
        if timeout 1 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/${TUNNEL_PORT}" 2>/dev/null; then
            echo -n "$text" | ssh -F /dev/null -p "${TUNNEL_PORT}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null 127.0.0.1 "pbcopy 2>/dev/null || clip.exe 2>/dev/null || xclip -selection clipboard 2>/dev/null || wl-copy 2>/dev/null" 2>/dev/null || true
            echo "Copied to host clipboard via tunnel!"
            exit 0
        fi
        echo "Error: Host bridge not reachable."
        exit 1
        ;;
    -h|--help)
        usage
        ;;
    *)
        cmd="$1"
        shift
        args_json="[]"
        if [ $# -gt 0 ]; then
            args_json=$(python3 -c "import json,sys; print(json.dumps(sys.argv[1:]))" "$@")
        fi
        payload=$(python3 -c "import json,sys,os; print(json.dumps({'cmd': sys.argv[1], 'args': json.loads(sys.argv[2]), 'display': os.environ.get('DISPLAY', ':1')}))" "$cmd" "$args_json")

        response=$(curl -s -f -X POST "${CP_URL}/api/host-bridge/exec" -H "Content-Type: application/json" -d "$payload" 2>/dev/null || true)
        if [ -n "$response" ]; then
            python3 -c "import json,sys; res=json.loads(sys.argv[1]); sys.stdout.write(res.get('stdout','')); sys.stderr.write(res.get('stderr','')); sys.exit(res.get('exit_code',0))" "$response"
            exit $?
        fi

        if timeout 1 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/${TUNNEL_PORT}" 2>/dev/null; then
            ssh -F /dev/null -p "${TUNNEL_PORT}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null 127.0.0.1 "DISPLAY=${DISPLAY:-:1} $cmd" "$@"
            exit $?
        fi
        echo "Error: Could not reach host bridge at ${CP_URL} to execute '${cmd}'."
        exit 1
        ;;
esac
`

	if err := writeStringToTar(tw, ".local/bin/host-run", hostRunScript, 0755); err != nil {
		return err
	}
	if err := writeStringToTar(tw, ".local/bin/host-open", "#!/usr/bin/env bash\nexec host-run open \"$@\"\n", 0755); err != nil {
		return err
	}
	if err := writeStringToTar(tw, ".local/bin/host-clip", "#!/usr/bin/env bash\nexec host-run clip \"$@\"\n", 0755); err != nil {
		return err
	}
	if err := writeStringToTar(tw, ".local/bin/win-run", "#!/usr/bin/env bash\nexec host-run \"$@\"\n", 0755); err != nil {
		return err
	}
	return writeStringToTar(tw, ".local/bin/mac-run", "#!/usr/bin/env bash\nexec host-run \"$@\"\n", 0755)
}

// -----------------------------------------------------------------------------
// Tar Archiving Helpers with Cache Filtering
// -----------------------------------------------------------------------------

// isCacheOrTransient returns true if a directory or file represents disposable caches,
// metrics, or transient crash dumps that cause RAM redundancy and page cache bloat.
func isCacheOrTransient(name, fullPath string, isDir bool) bool {
	lower := strings.ToLower(name)
	lowerPath := strings.ToLower(fullPath)

	// Strictly exclude self / app repository to prevent recursive nesting
	if strings.Contains(lowerPath, "flowera") || strings.Contains(lowerPath, "migration-toolkit") {
		return true
	}

	// Browser and toolchain caches
	if strings.Contains(lower, "cache") {
		return true
	}
	if strings.HasPrefix(lower, "cached") {
		return true
	}
	if lower == "gpu-cache" || lower == "gpucache" || lower == "code cache" || lower == "codecache" || lower == "dawncache" {
	if lower == "gpu-cache" || lower == "gpucache" || lower == "code cache" || lower == "codecache" || lower == "dawncache" || lower == "grshadercache" {
		return true
	}
	if lower == "crashpad" || lower == "crash reports" || strings.HasPrefix(lower, "browsermetrics") {
	if lower == "crashpad" || lower == "crash reports" || strings.HasPrefix(lower, "browsermetrics") || lower == "minidumps" {
		return true
	}
	if strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".dmp") {
	if strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".dmp") || strings.HasSuffix(lower, ".swp") {
		return true
	}
	if isDir && (lower == "logs" || lower == "node_modules" || lower == "pkg" || lower == "temp" ||
		lower == "indexeddb" || lower == "service worker" || lower == "blob_storage" || lower == "file system" ||
		lower == "storage" || lower == "extensions" || lower == "dictionaries" || lower == "media cache" ||
		lower == "optimizationguide" || lower == "hyphenation-data" || lower == "variations" || lower == "webrtc_event_logs") {
		lower == "optimizationguide" || lower == "hyphenation-data" || lower == "variations" || lower == "webrtc_event_logs" ||
		lower == ".git" || lower == "__pycache__" || lower == ".pytest_cache" || lower == ".mypy_cache" || lower == ".cache") {
		return true
	}

	return false
}

// writeDirToTar recursively walks a source directory on the host and writes its
// contents to the tar archive under destPrefix, filtering through excludeFilter.
// It limits directory depth to 3 levels and caps total transferred bytes to 35MB
// to keep desktop creation instantaneous.
func writeDirToTar(tw *tar.Writer, srcDir, destPrefix string, excludeFilter func(name, fullPath string, isDir bool) bool) error {
	srcClean := filepath.Clean(srcDir)
	var totalWritten int64
	const maxArchiveBytes = 35 * 1024 * 1024

	return filepath.Walk(srcClean, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}

		rel, err := filepath.Rel(srcClean, path)
		if err != nil || rel == "." {
			return nil
		}

		isDir := info.IsDir()
		if isDir {
			// Limit depth to avoid recursing deeply into huge trees
			if strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
		}

		// Skip non-regular special files (sockets, pipes, devices)
		if !isDir && (info.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice) != 0) {
			return nil
		}

		if excludeFilter != nil && excludeFilter(info.Name(), path, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		if totalWritten > maxArchiveBytes {
			return filepath.SkipAll
		}

		// Enforce size safety limit (skip individual files > 10MB to avoid sluggish transfers)
		if !isDir && info.Size() > 10*1024*1024 {
			return nil
		}

		tarPath := filepath.ToSlash(filepath.Join(destPrefix, rel))

		var linkTarget string
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err == nil {
				linkTarget = target
			}
		}

		header, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return nil
		}

		header.Name = tarPath
		header.Uid = 1000
		header.Gid = 1000
		header.Uname = "headless"
		header.Gname = "headless"

		isSSH := destPrefix == ".ssh" || strings.HasPrefix(destPrefix, ".ssh/")
		isKeyring := strings.Contains(destPrefix, "keyrings")

		if isDir {
			if isSSH || isKeyring {
				header.Mode = 0700
			} else {
				header.Mode = 0755
			}
		} else if info.Mode().IsRegular() {
			if isSSH || isKeyring {
				header.Mode = 0600
			} else if info.Mode()&0111 != 0 {
				header.Mode = 0755
			} else {
				header.Mode = 0644
			}
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			n, errCopy := io.Copy(tw, f)
			_ = f.Close()
			if errCopy != nil {
				return errCopy
			}
			totalWritten += n
		}

		return nil
	})
}

// writeFileToTar writes a single host file into the tar archive at tarPath.
func writeFileToTar(tw *tar.Writer, srcPath, tarPath string, mode int64) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}

	header := &tar.Header{
		Name:     tarPath,
		Mode:     mode,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeReg,
		Uid:      1000,
		Gid:      1000,
		Uname:    "headless",
		Gname:    "headless",
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(tw, f)
	return err
}

// writeStringToTar writes a virtual string into the tar archive at tarPath with given mode.
func writeStringToTar(tw *tar.Writer, tarPath, content string, mode int64) error {
	data := []byte(content)
	header := &tar.Header{
		Name:     tarPath,
		Mode:     mode,
		Size:     int64(len(data)),
		ModTime:  time.Now(),
		Typeflag: tar.TypeReg,
		Uid:      1000,
		Gid:      1000,
		Uname:    "headless",
		Gname:    "headless",
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	_, err := tw.Write(data)
	return err
}

// HostOS returns the host operating system ("linux", "darwin", "windows").
func HostOS() string {
	return runtime.GOOS
}
