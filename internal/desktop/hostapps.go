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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
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
	// Mode says how the app is shown in a VM: ModeWeb, ModeHostGUI, ModeData or ModeHelper.
	Mode            string `json:"mode,omitempty"`
	HostCmd         string `json:"host_cmd,omitempty"`
	Icon            string `json:"icon,omitempty"`
	TargetConfigDir string `json:"target_config_dir,omitempty"`
	// UnavailableReason is set when discovery found the app but Decurix cannot
	// safely show it inside a desktop. Keeping the item in discovery makes the
	// scan complete without presenting a launcher that cannot work.
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	// Unverified marks an unavailable app the host was not asked about successfully (its helper died or timed out) -
	// "not checked yet", which a later check may turn into a yes, unlike a verdict the host actually gave.
	Unverified bool `json:"unverified,omitempty"`
}

// knownAppDef defines how to probe and migrate a specific application or session.
type knownAppDef struct {
	id              string
	name            string
	category        string
	description     string
	defaultSelected bool
	probeFn         func() []string
	packFn          func(tw *tar.Writer, paths []string, pc packContext) error
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
			res := slices.Clone(cachedHostApps)
			hostAppsCacheMu.RUnlock()
			return withExtraHostApps(res)
		}
		hostAppsCacheMu.RUnlock()
	}

	hostAppsCacheMu.Lock()
	defer hostAppsCacheMu.Unlock()

	if !force && len(cachedHostApps) > 0 && time.Since(cachedHostTime) < 3*time.Minute {
		return withExtraHostApps(slices.Clone(cachedHostApps))
	}

	if force {
		forgetHostVerdicts()
		forgetStartApps()
	}
	results := runDetectHostApps()
	cachedHostApps = results
	cachedHostTime = time.Now()

	return withExtraHostApps(slices.Clone(results))
}

// DetectHostApps scans the host machine for credentials and settings that can be copied, web apps, and Windows programs.
// Every discovered Windows app is returned; vetHostApps marks the ones that cannot safely run in a VM as unavailable.
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

		appType, mode := "app_session", ModeData
		switch def.id {
		case "chrome", "firefox":
			appType = "browser_session"
		case "claude", "chatgpt":
			mode = ModeWeb
		case "ssh", "gh", "docker", "gcloud", "azure":
			appType = "credential"
		case "host_bridge":
			mode = ModeHelper
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
			Mode:            mode,
			Icon:            pickAppIcon(def.name),
		})
	}

	// Dynamically explore all other apps installed on Windows, Linux, or macOS
	for _, dapp := range exploreDynamicApps(seen) {
		if dapp.Detected {
			items = append(items, dapp)
		}
	}

	// Sort items alphabetically by Name
	sort.SliceStable(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	return items
}

// How an app that was copied into a VM shows up there.
const (
	// ModeWeb opens the service's web version in the VM's own browser; nothing runs on the host.
	ModeWeb = "web"
	// ModeHostGUI runs the program on a private desktop of the host and shows and drives it from the VM.
	ModeHostGUI = "host_gui"
	// ModeData copies credentials or settings as files; there is nothing to launch.
	ModeData = "data"
	// ModeHelper is the command-line bridge every copy installs.
	ModeHelper = "helper"
	// ModeUnavailable is still a discovered app, but not one the desktop can
	// launch. The UI shows it disabled together with UnavailableReason.
	ModeUnavailable = "unavailable"
	// ModePhone runs the app on a linked Android phone, on a display of its
	// own, and shows and drives it from the VM (internal/phonehost).
	ModePhone = "phone"
)

// phoneCmdPrefix marks a HostCmd that names an app on a linked phone:
// "android:<paired device id>/<package>".
const phoneCmdPrefix = "android:"

// PhoneCmd is the HostCmd of package pkg on the phone paired as device.
func PhoneCmd(device, pkg string) string {
	return phoneCmdPrefix + device + "/" + pkg
}

// parsePhoneCmd splits a PhoneCmd back into the phone and the package.
func parsePhoneCmd(cmd string) (device, pkg string, ok bool) {
	rest, found := strings.CutPrefix(cmd, phoneCmdPrefix)
	if !found {
		return "", "", false
	}

	device, pkg, ok = strings.Cut(rest, "/")
	return device, pkg, ok && device != "" && pkg != ""
}

var (
	extraHostAppsMu sync.RWMutex
	extraHostApps   func() []HostAppItem
)

// SetExtraHostApps adds apps found somewhere other than this machine - the
// apps of linked phones - to every scan's answer. f is called on every scan
// and must answer at once (from what is already known), not go and look.
func SetExtraHostApps(f func() []HostAppItem) {
	extraHostAppsMu.Lock()
	defer extraHostAppsMu.Unlock()

	extraHostApps = f
}

// withExtraHostApps appends the extra apps to a scan's result.
func withExtraHostApps(items []HostAppItem) []HostAppItem {
	extraHostAppsMu.RLock()
	f := extraHostApps
	extraHostAppsMu.RUnlock()

	if f == nil {
		return items
	}

	return append(items, f()...)
}

// HostAppChecker asks the host which Windows launch targets can run on a private desktop, and why not for the rest.
type HostAppChecker func(ctx context.Context, targets []winhost.Target) (map[winhost.Target]string, error)

var (
	hostAppCheckerMu sync.RWMutex
	hostAppChecker   HostAppChecker

	// hostVerdicts remembers what the host said about each target until a rescan, so the periodic refresh of the list
	// does not start the helper again for apps it already knows.
	hostVerdictsMu sync.Mutex
	hostVerdicts   = map[winhost.Target]string{}
)

// hostAppWarmer, when set, gets the host ready to show an app before anyone has asked for one.
var (
	hostAppWarmerMu sync.RWMutex
	hostAppWarmer   func()
)

// SetHostAppWarmer installs what WarmHostApps calls.
func SetHostAppWarmer(f func()) {
	hostAppWarmerMu.Lock()
	hostAppWarmer = f
	hostAppWarmerMu.Unlock()
}

// WarmHostApps gets the host ready to run an app hidden - starting the helper and everything it has to compile or load -
// so that nobody waits for it when they open one. It returns at once; call it when an app is likely to be wanted soon,
// such as while a desktop that will show host apps is still starting up.
func WarmHostApps() {
	hostAppWarmerMu.RLock()
	f := hostAppWarmer
	hostAppWarmerMu.RUnlock()
	if f != nil {
		f()
	}
}

// hostAppStarter, when set, keeps one app running on a private host desktop ahead of time - under the same session id
// its packaged launcher will ask for - so the window, and everything a cold start on the host costs (a browser's first
// paint, a hidden profile's own first run), is already paid for before a person ever opens it. desktopID is the VM the
// app was copied to, so its apps can be let go when it is deleted.
var (
	hostAppStarterMu sync.RWMutex
	hostAppStarter   func(desktopID, id, target, args string)
)

// SetHostAppStarter installs what packDynamicApp calls, in the background, for every host_gui app it packages.
func SetHostAppStarter(f func(desktopID, id, target, args string)) {
	hostAppStarterMu.Lock()
	hostAppStarter = f
	hostAppStarterMu.Unlock()
}

// startHostAppAhead fires the installed starter, if any, and never blocks: packaging a launcher must not wait on the
// app it names actually opening on the host.
func startHostAppAhead(desktopID, id, target, args string) {
	hostAppStarterMu.RLock()
	f := hostAppStarter
	hostAppStarterMu.RUnlock()
	if f != nil {
		f(desktopID, id, target, args)
	}
}

// hostAppPage, when set, says which page a checked Windows target only opens: a web app's launcher for a page on this
// computer (winhost.Manager.LocalPage), "" for anything else.
var (
	hostAppPageMu sync.RWMutex
	hostAppPage   func(target, args string) string
)

// SetHostAppPageFinder installs what localPageFor asks.
func SetHostAppPageFinder(f func(target, args string) string) {
	hostAppPageMu.Lock()
	hostAppPage = f
	hostAppPageMu.Unlock()
}

// localPageFor is the page on this computer that a Windows app only opens in a browser, or "".
func localPageFor(target, args string) string {
	hostAppPageMu.RLock()
	f := hostAppPage
	hostAppPageMu.RUnlock()
	if f == nil {
		return ""
	}
	return f(target, args)
}

// vmPageURL is page as a VM reaches it: the host's loopback address becomes host.docker.internal, which every desktop
// resolves to the machine it runs on (the same name CONTROL_PLANE_URL uses). "" when page is not a loopback address.
func vmPageURL(page string) string {
	u, err := url.Parse(page)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
	default:
		return ""
	}
	host := "host.docker.internal"
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	u.Host = host
	return u.String()
}

// SetHostAppChecker installs the way Windows apps are checked. Without one, no Windows program is offered - one that
// cannot be checked is not one that can be promised.
func SetHostAppChecker(c HostAppChecker) {
	hostAppCheckerMu.Lock()
	hostAppChecker = c
	hostAppCheckerMu.Unlock()
	forgetHostVerdicts()
}

func forgetHostVerdicts() {
	hostVerdictsMu.Lock()
	hostVerdicts = map[winhost.Target]string{}
	hostVerdictsMu.Unlock()
}

// checkHostTargets returns the host's verdict for each target: "" when it can run on a private desktop, otherwise why
// not. A target the host could not be asked about is absent from the result.
func checkHostTargets(targets []winhost.Target) map[winhost.Target]string {
	out := make(map[winhost.Target]string, len(targets))
	var unknown []winhost.Target
	hostVerdictsMu.Lock()
	for _, t := range targets {
		if v, ok := hostVerdicts[t]; ok {
			out[t] = v
		} else {
			unknown = append(unknown, t)
		}
	}
	hostVerdictsMu.Unlock()
	if len(unknown) == 0 {
		return out
	}

	hostAppCheckerMu.RLock()
	check := hostAppChecker
	hostAppCheckerMu.RUnlock()
	if check == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	got, err := check(ctx, unknown)
	if err != nil {
		log.Printf("hostapps: could not check Windows apps on the host (%d of %d checked): %v", len(got), len(unknown), err)
	}
	hostVerdictsMu.Lock()
	for t, v := range got {
		hostVerdicts[t] = v
		out[t] = v
	}
	hostVerdictsMu.Unlock()
	return out
}

// needsSound reports whether an app is mainly for playing audio or video. The desktop VMs have no sound device, so such
// an app would open, look right and say nothing.
func needsSound(name string) bool {
	return categorizeAppName(name) == "Media & Entertainment"
}

// vetHostApps says how each discovered Windows app can be used. A service with a web version opens in the VM's browser;
// anything else must be a program the host confirms it can run on a private desktop. Apps that cannot be launched remain
// visible as disabled discovery results, with the reason, instead of silently disappearing from the scan.
func vetHostApps(apps []HostAppItem) []HostAppItem {
	var kept []HostAppItem
	var pending []HostAppItem
	var targets []winhost.Target
	for _, app := range apps {
		if needsSound(app.Name) {
			app.Mode = ModeUnavailable
			app.UnavailableReason = "This desktop has no audio device, so this app cannot be used here."
			app.Description = app.UnavailableReason
			kept = append(kept, app)
			continue
		}
		if getWebAppURLForApp(app.Name, app.HostCmd) != "" {
			app.Mode = ModeWeb
			app.Description = "Opens the web version in the VM's browser; you sign in there"
			kept = append(kept, app)
			continue
		}
		target, args, ok := windowsTarget(app.HostCmd)
		if !ok {
			if app.AppType == "desktop_app" {
				app.Mode = ModeHostGUI
				app.Description = "Runs on this computer; you see and use it in the VM"
				kept = append(kept, app)
				continue
			}
			app.Mode = ModeUnavailable
			app.UnavailableReason = "Decurix could not resolve a safe hidden-host launch command for this app."
			app.Description = app.UnavailableReason
			kept = append(kept, app)
			continue
		}
		pending = append(pending, app)
		targets = append(targets, winhost.Target{Target: target, Args: args})
	}
	if len(pending) == 0 {
		return kept
	}

	verdicts := checkHostTargets(targets)
	hostAppCheckerMu.RLock()
	canAsk := hostAppChecker != nil
	hostAppCheckerMu.RUnlock()
	for i, app := range pending {
		app.Unverified, app.UnavailableReason = false, ""
		reason, checked := verdicts[targets[i]]
		if !checked && !canAsk {
			reason = "This computer has no way to run Windows apps for a desktop."
		} else if !checked {
			// Not a "no": the host was not asked successfully (found live 2026-10-20, a helper that died partway
			// right after a control-plane start). A desktop that asked for it keeps trying (retryPendingApps).
			reason = UnverifiedAppReason
			app.Unverified = true
		}
		if reason != "" {
			app.Mode = ModeUnavailable
			app.UnavailableReason = reason
			app.Description = reason
			kept = append(kept, app)
			continue
		}
		app.Mode = ModeHostGUI
		if localPageFor(targets[i].Target, targets[i].Args) != "" {
			app.Description = "A web app: opens in the VM's browser, its server runs on this computer"
		} else if isBrowserApp(app.Name) {
			app.Description = "Runs privately on this computer; your signed-in browser profile is copied the first time it opens"
		} else {
			app.Description = "Runs on this computer on a hidden desktop; you see and use it in the VM"
		}
		kept = append(kept, app)
	}
	return kept
}

func isBrowserApp(name string) bool {
	lower := strings.ToLower(name)
	for _, browser := range []string{"chrome", "chromium", "edge", "brave", "vivaldi", "opera"} {
		if strings.Contains(lower, browser) {
			return true
		}
	}
	return false
}

// MigrateHostApps packages selected host application sessions, tokens, and configurations,
// strips cache bloat to maintain zero redundant memory, streams them into the container's
// /home/headless volume, and installs the host execution bridge launchers. A requested app it cannot add is never
// skipped silently: it is recorded with the reason (MissingApps, the desktop's card, and a file its agent reads), and
// one the host could not be asked about yet is tried again in the background and added when it checks out.
func (m *Manager) MigrateHostApps(ctx context.Context, desktopID string, selectedIDs []string) error {

	if len(selectedIDs) == 0 {
		return nil
	}
	// The desktop is about to (re)start and show these apps; whatever the host has to build for them can happen meanwhile.
	WarmHostApps()

	sanitizedID := sanitizeName(desktopID)
	containerName := "desktop_" + sanitizedID

	// Inspect container to ensure it exists
	inspect, err := m.client.ContainerInspect(ctx, containerName)
	if err != nil {
		return fmt.Errorf("inspect %s for migration: %w", containerName, err)
	}

	selectedMap := make(map[string]bool)
	requested := make(map[string]bool)
	for _, id := range selectedIDs {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			selectedMap[id] = true
			requested[id] = true
		}
	}

	// Always include host_bridge when apps are being migrated
	selectedMap["host_bridge"] = true

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
	// An app the host could not be asked about when the list was made is asked about again now, before it is called
	// missing: the list may be minutes old, and its helper may have died since recovered.
	var recheck []HostAppItem
	for id := range selectedMap {
		if app, ok := appMap[id]; ok && app.Unverified {
			recheck = append(recheck, app)
		}
	}
	for _, app := range recheckHostApps(recheck) {
		appMap[strings.ToLower(app.ID)] = app
	}

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

	migratedCount := 0
	var missing []MissingApp
	miss := func(id, name, reason string, pending bool) {
		if requested[id] {
			missing = append(missing, MissingApp{ID: id, Name: name, Reason: reason, Pending: pending})
		}
	}

	for id := range selectedMap {
		if def, ok := defMap[id]; ok {
			var paths []string
			if appItem, found := appMap[id]; found && len(appItem.DetectedPaths) > 0 {
				paths = appItem.DetectedPaths
			} else if def.probeFn != nil {
				paths = def.probeFn()
			}
			if len(paths) == 0 && def.id != "host_bridge" {
				miss(id, def.name, "Decurix found nothing of it on this computer to copy.", false)
				continue
			}
			if err := def.packFn(tw, paths, packContext{DesktopID: sanitizedID, Secret: m.HostGUISecret}); err != nil {
				log.Printf("hostapps: packing %s for %s: %v", def.id, desktopID, err)
				miss(id, def.name, "Copying it into the desktop failed.", false)
				continue
			}
			migratedCount++
		} else if app, ok := appMap[id]; ok {
			if app.Mode == ModeUnavailable {
				log.Printf("hostapps: not adding %s to %s (pending=%v): %s", app.ID, desktopID, app.Unverified, app.UnavailableReason)
				miss(id, app.Name, app.UnavailableReason, app.Unverified)
				continue
			}
			if err := packDynamicApp(tw, app, packContext{DesktopID: sanitizedID, Secret: m.HostGUISecret}); err != nil {
				log.Printf("hostapps: packing dynamic app %s for %s: %v", app.ID, desktopID, err)
				miss(id, app.Name, "Copying it into the desktop failed.", false)
				continue
			}
			migratedCount++
		} else {
			miss(id, id, "Decurix did not find this app on this computer.", false)
		}
	}

	if err := tw.Close(); err != nil {
		_ = pw.CloseWithError(err)
		<-copyErrCh
		return fmt.Errorf("close tar archive: %w", err)
	}
	_ = pw.Close()

	if err := <-copyErrCh; err != nil {
		return fmt.Errorf("copy host apps to %s: %w", containerName, err)
	}

	log.Printf("hostapps: successfully migrated %d component(s) into %s", migratedCount, containerName)
	if m.noteMissingApps(sanitizedID, requested, missing) {
		if err := writeMissingFileFn(m, ctx, inspect.ID, m.MissingApps(sanitizedID)); err != nil {
			log.Printf("hostapps: telling %s which apps are missing: %v", containerName, err)
		}
	}
	m.retryPendingApps(sanitizedID)
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
	if envHome := os.Getenv("DECURIX_HOST_HOME"); envHome != "" {
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

	if d := os.Getenv("DECURIX_HOST_MNT_C"); d != "" {
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

// getKnownAppDefs is the apps a desktop can be given launchers for.
func getKnownAppDefs() []knownAppDef {
	homes := getHostHomeCandidates()
	cDrives := getWindowsCDriveCandidates()

	// Only launchers are copied in: never a browser profile, keys, tokens, keyrings or dotfiles of the person's own
	// accounts. A worker that held them could act as the person anywhere, and anything that can steer the worker (a
	// web page, an email) could take them (review 2026-11-02). A worker signs in to what it needs on its own screen.
	return []knownAppDef{
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
					found = append(found, usersGlob(cDrive, "AppData", "Roaming", "Claude")...)
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string, pc packContext) error {
				return packWebAppHost(tw, pc, "claude", "Claude", "https://claude.ai")
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
					found = append(found, usersGlob(cDrive, "AppData", "Roaming", "ChatGPT")...)
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string, pc packContext) error {
				return packWebAppHost(tw, pc, "chatgpt", "ChatGPT", "https://chatgpt.com")
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
			packFn: func(tw *tar.Writer, paths []string, pc packContext) error {
				return packHostBridgeLaunchers(tw)
			},
		},
	}
}

// usersGlob is rel under every Windows user's folder on the drive root cDrive.
func usersGlob(cDrive string, rel ...string) []string {
	matches, _ := filepath.Glob(filepath.Join(append([]string{cDrive, "Users", "*"}, rel...)...))
	return matches
}

// existingDirs is those of paths that are directories.
func existingDirs(paths []string) []string {
	var dirs []string
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// packFirstDir packs the first probed directory as dest, leaving out what exclude says.
func packFirstDir(dest string, exclude func(name, fullPath string, isDir bool) bool) func(*tar.Writer, []string) error {
	return func(tw *tar.Writer, paths []string) error {
		if len(paths) == 0 {
			return nil
		}
		return writeDirToTar(tw, paths[0], dest, exclude)
	}
}

// packWebAppHost packs a host AI app (slug, shown as name) as a launcher that opens site in an app window, or with no
// browser in the desktop, in the person's own browser: only site, signed for this desktop (winhost.OpenToken).
func packWebAppHost(tw *tar.Writer, pc packContext, slug, name, site string) error {
	fallback := "echo \"No browser is installed in this desktop to open " + site + " in.\" >&2\nexit 1\n"
	if len(pc.Secret) > 0 {
		fallback = fmt.Sprintf("exec /home/headless/.local/bin/host-run open %s %s\n", winhost.OpenToken(pc.Secret, pc.DesktopID, site), shellQuote(site))
	}
	wrapper := fmt.Sprintf(`#!/usr/bin/env bash
if command -v chromium-browser >/dev/null 2>&1; then
    exec chromium-browser --app="%[1]s" "$@"
elif command -v google-chrome >/dev/null 2>&1; then
    exec google-chrome --app="%[1]s" "$@"
elif command -v chromium >/dev/null 2>&1; then
    exec chromium --app="%[1]s" "$@"
fi
%[2]s`, site, fallback)
	if err := writeStringToTar(tw, ".local/bin/"+slug+"-host", wrapper, 0755); err != nil {
		return err
	}
	if iconSlug, svg := getAppSVGIcon(slug); svg != "" {
		_ = writeStringToTar(tw, ".local/share/icons/hicolor/scalable/apps/"+iconSlug+".svg", svg, 0644)
	}
	desktop := fmt.Sprintf(`[Desktop Entry]
Version=1.0
Type=Application
Name=%[2]s (Host)
Comment=Run %[2]s directly in VM desktop with zero host desktop popup
Exec=/home/headless/.local/bin/%[1]s-host %%U
Icon=/home/headless/.local/share/icons/hicolor/scalable/apps/%[1]s.svg
Terminal=false
Categories=Office;Utility;Network;
`, slug, name)
	return writeHostLauncher(tw, slug, desktop)
}

// writeHostLauncher puts an app's .desktop entry on the VM's desktop and in its applications menu.
func writeHostLauncher(tw *tar.Writer, slug, entry string) error {
	if err := writeStringToTar(tw, "Desktop/"+slug+"-host.desktop", entry, 0755); err != nil {
		return err
	}
	return writeStringToTar(tw, ".local/share/applications/"+slug+"-host.desktop", entry, 0755)
}

// -----------------------------------------------------------------------------
// Dynamic Discovery Providers (Windows/WSL, Linux, macOS)
// -----------------------------------------------------------------------------

// IsWSLOrWindows returns true if running directly on Windows or inside a WSL environment, or inside a container on such
// a host (the kernel it shares says so, or a mounted drive really holds Windows).
func IsWSLOrWindows() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	if os.Getenv("WSLENV") != "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	if data, err := os.ReadFile("/proc/version"); err == nil {
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl") {
			return true
		}
	}
	return hasWindowsInstall(getWindowsCDriveCandidates())
}

// hasWindowsInstall reports whether any of the drive roots holds a Windows installation. A folder merely existing at
// /mnt/c is no evidence of one: docker-compose.yaml always creates that mount, and on a Mac or a Linux machine it is an
// empty folder (or the host's own root), which used to make those hosts believe they were Windows.
func hasWindowsInstall(drives []string) bool {
	for _, drive := range drives {
		for _, name := range []string{"Windows", "WINDOWS", "windows"} {
			if fi, err := os.Stat(filepath.Join(drive, name)); err == nil && fi.IsDir() {
				return true
			}
		}
	}
	return false
}

type rawStartApp struct {
	Name  string `json:"Name"`
	AppID string `json:"AppID"`
}

func forgetStartApps() {
	startAppsMu.Lock()
	startAppsCached = nil
	startAppsQueried = false
	startAppsMu.Unlock()
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

// PowershellExe returns the path of Windows PowerShell, or "" when this host has none.
func PowershellExe() string {
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
	// Nothing to ask on a Mac or a Linux machine - and the last resort below starts a container.
	if !IsWSLOrWindows() {
		return nil
	}
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
	for _, cachePath := range []string{"/tmp/decurix_host_apps.json", "/data/decurix_host_apps.json"} {
		if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 {
			if apps := parseGetStartAppsJSON(data); len(apps) > 0 {
				startAppsCached = apps
				startAppsQueried = true
				return apps
			}
		}
	}

	// 2. Direct execution of powershell.exe with -InputFormat None and empty stdin
	psExe := PowershellExe()
	if psExe != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, psExe, "-NoProfile", "-NonInteractive", "-InputFormat", "None", "-Command", "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress")
		cmd.Stdin = strings.NewReader("")
		HideWindow(cmd)
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
				cmd := HopCommand(ctx, dockerBin, "start-apps", false, psCmd)
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
		roots = append(roots, existingDirs([]string{
			filepath.Join(drive, "ProgramData", "Microsoft", "Windows", "Start Menu", "Programs"),
			filepath.Join(drive, "ProgramData", "Microsoft", "WINDOWS", "Start Menu", "Programs"),
		})...)

		// User Roaming start menu
		roots = append(roots, usersGlob(drive, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs")...)
		roots = append(roots, usersGlob(drive, "AppData", "Roaming", "Microsoft", "WINDOWS", "Start Menu", "Programs")...)

		// User Local Programs (e.g. Cursor, VS Code, Bruno, agent-orchestrator)
		roots = append(roots, usersGlob(drive, "AppData", "Local", "Programs")...)

		// User Desktop shortcuts
		roots = append(roots, usersGlob(drive, "Desktop")...)
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

			id := "win_" + sanitizeAppSlug(cleanName)
			if !claimApp(seen, cleanName, id) {
				return nil
			}
			hostCmd := fmt.Sprintf(`cmd.exe /c start "" "%s"`, wslToWindowsPath(path))

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

// scanInstalledWindowsBrowsers is a fallback for machines where Get-StartApps is unavailable or a browser did not add a
// Start-menu shortcut. Browsers matter especially because the private-host launcher can initialize from the person's
// signed-in profile; losing Chrome merely because shell discovery failed makes that profile impossible to select.
func scanInstalledWindowsBrowsers(seen map[string]bool) []HostAppItem {
	type browserInstall struct {
		name     string
		patterns []string
	}
	var items []HostAppItem
	for _, drive := range getWindowsCDriveCandidates() {
		installs := []browserInstall{
			{"Google Chrome", []string{
				filepath.Join(drive, "Program Files", "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(drive, "Program Files (x86)", "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(drive, "Users", "*", "AppData", "Local", "Google", "Chrome", "Application", "chrome.exe"),
			}},
			{"Microsoft Edge", []string{
				filepath.Join(drive, "Program Files (x86)", "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(drive, "Program Files", "Microsoft", "Edge", "Application", "msedge.exe"),
			}},
			{"Brave", []string{
				filepath.Join(drive, "Program Files", "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
				filepath.Join(drive, "Users", "*", "AppData", "Local", "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
			}},
			{"Vivaldi", []string{
				filepath.Join(drive, "Users", "*", "AppData", "Local", "Vivaldi", "Application", "vivaldi.exe"),
				filepath.Join(drive, "Program Files", "Vivaldi", "Application", "vivaldi.exe"),
			}},
		}
		for _, install := range installs {
			key, id := strings.ToLower(install.name), "win_"+sanitizeAppSlug(install.name)
			if seen[key] || seen[id] {
				continue
			}
			var executable string
			for _, pattern := range install.patterns {
				matches, _ := filepath.Glob(pattern)
				for _, match := range matches {
					if fi, err := os.Stat(match); err == nil && !fi.IsDir() {
						executable = match
						break
					}
				}
				if executable != "" {
					break
				}
			}
			if executable == "" {
				continue
			}
			seen[key], seen[id] = true, true
			items = append(items, HostAppItem{
				ID: id, Name: install.name, Category: "Browsers & Web",
				Description: "Windows browser", Detected: true,
				DetectedPaths: []string{executable}, AppType: "windows_app",
				HostCmd: fmt.Sprintf(`cmd.exe /c start "" "%s"`, wslToWindowsPath(executable)),
				Icon:    pickAppIcon(install.name),
			})
		}
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

			id := "linux_" + sanitizeAppSlug(meta.Name)
			if !claimApp(seen, meta.Name, id) {
				continue
			}

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
			id := "mac_" + sanitizeAppSlug(appName)
			if !claimApp(seen, appName, id) {
				continue
			}
			fullPath := filepath.Join(dir, e.Name())

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

// claimApp marks an app's name (case-folded) and then its id as found, and reports false - leaving the app out - as soon
// as either of them already was.
func claimApp(seen map[string]bool, name, id string) bool {
	key := strings.ToLower(name)
	if seen[key] {
		return false
	}
	seen[key] = true
	if seen[id] {
		return false
	}
	seen[id] = true
	return true
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
			id := "win_" + sanitizeAppSlug(app.Name)
			if !claimApp(seen, app.Name, id) {
				continue
			}

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

		// Get-StartApps can be absent or incomplete; standard browser installs still need to expose the signed-in profile.
		items = append(items, scanInstalledWindowsBrowsers(seen)...)

		// Windows Start Menu (.lnk shortcuts including Chrome Apps & Edge Apps)
		menuApps := scanWindowsStartMenu(seen)
		items = append(items, menuApps...)
	}

	// 2. Linux Desktop files (.desktop)
	items = append(items, scanLinuxDesktopFiles(seen)...)

	// 3. macOS App Bundles (.app)
	items = append(items, scanMacAppBundles(seen)...)

	return vetHostApps(items)
}

// splitWords splits a host command line the way Windows does: whitespace separates words, a double quote groups
// them (an empty pair is an empty word), and a backslash is just a character.
func splitWords(cmd string) []string {
	var words []string
	var cur strings.Builder
	inQuotes, started := false, false
	flush := func() {
		if started {
			words = append(words, cur.String())
		}
		cur.Reset()
		started = false
	}
	for _, r := range cmd {
		switch {
		case r == '"':
			inQuotes, started = !inQuotes, true
		case !inQuotes && (r == ' ' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return words
}

var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellWords returns a host command line's words single-quoted for a bash wrapper. Pasting the raw line into the
// wrapper let bash eat the backslash in `shell:AppsFolder\Microsoft.WindowsCalculator...`, so the host was asked to
// open a name that does not exist and silently did nothing.
func shellWords(cmd string) string {
	words := splitWords(cmd)
	for i, w := range words {
		if !shellSafeWord.MatchString(w) {
			words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// windowsTarget extracts what a "cmd.exe /c start ..." host command runs: the program, shortcut or shell: address, and
// its arguments.
func windowsTarget(hostCmd string) (target, args string, ok bool) {
	w := splitWords(hostCmd)
	if len(w) < 4 || !strings.EqualFold(w[0], "cmd.exe") || w[1] != "/c" || !strings.EqualFold(w[2], "start") {
		return "", "", false
	}
	rest := w[3:]
	if len(rest) > 0 && rest[0] == "" { // start's optional window-title argument
		rest = rest[1:]
	}
	if len(rest) > 0 && strings.EqualFold(rest[0], "explorer.exe") {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return "", "", false
	}
	return rest[0], strings.Join(rest[1:], " "), true
}

// packContext is what a launcher needs to know about the desktop it is being written into.
type packContext struct {
	DesktopID string
	// Secret signs the launch token (winhost.Token); empty leaves the launcher unsigned, which the control plane refuses.
	Secret []byte
}

// sessionID names the host session for an app on this desktop, so two desktops never share one app.
func (pc packContext) sessionID(slug string) string {
	id := pc.DesktopID
	if id == "" {
		id = "vm"
	}
	if len(slug) > 24 {
		slug = slug[:24]
	}
	if id = id + "-" + slug; len(id) > 64 {
		id = id[:64]
	}
	return id
}

// hostGUILauncher is the shell that shows a Windows app which runs on the host with no window there: it opens the
// control plane's viewer in an app window of the VM's own browser, and the viewer shows the app and sends input back.
// The URL is fixed when the launcher is written and carries a token signed for exactly this app, so the control plane
// will start it and nothing else.
//
// Like webAppWrapper it raises the viewer when it is already open and ignores clicks while one is opening: the VM's
// browser shows nothing until its window exists (seconds on a cold 1-vCPU desktop), and every impatient click used to
// open another viewer of the same app. The viewer's window is found by its class (Chromium names an --app window after
// the URL's host and path: "<host>__host-gui_view") and its title (the app's name). With no X display or no xdotool
// (tests, a broken session) it just opens the viewer, as it always did.
func hostGUILauncher(pc packContext, slug, title, target, args string) string {
	id := pc.sessionID(slug)
	q := url.Values{"id": {id}, "desktop": {pc.DesktopID}, "title": {title}, "target": {target}, "args": {args}}
	if len(pc.Secret) > 0 {
		q.Set("token", winhost.Token(pc.Secret, id, target, args))
	}
	return fmt.Sprintf(`CP="${CONTROL_PLANE_URL:-http://host.docker.internal:8080}"
VIEW="$CP/host-gui/view?%s"
TITLE=%s
export DISPLAY="${DISPLAY:-:1}"

viewer_window() {
    for w in $(xdotool search --classname '__host-gui_view$' 2>/dev/null); do
        [ "$(xdotool getwindowname "$w" 2>/dev/null)" = "$TITLE" ] && { echo "$w"; return 0; }
    done
    return 1
}

for browser in chromium-browser google-chrome chromium; do
    command -v "$browser" >/dev/null 2>&1 || continue
    if ! command -v xdotool >/dev/null 2>&1 || ! xdotool getdisplaygeometry >/dev/null 2>&1; then
        exec "$browser" --app="$VIEW"
    fi
    # Already open (possibly minimized): bring it forward instead of opening a second viewer.
    if win=$(viewer_window) && [ -n "$win" ]; then
        xdotool windowactivate "$win" >/dev/null 2>&1
        exit 0
    fi
    # A launch is already in flight: this click is a repeat of it.
    exec 9>/tmp/decurix-launch-%s.lock
    flock -n 9 || exit 0
    # 9>&- : the browser must not inherit the lock, or a cold start would hold it for as long as it runs.
    "$browser" --app="$VIEW" >/dev/null 2>&1 9>&- &
    # Hold the lock until the viewer is up (bounded), so clicks in the meantime are ignored.
    for _ in $(seq 1 900); do
        viewer_window >/dev/null && break
        sleep 0.1
    done
    exit 0
done
echo "No browser is installed in this desktop to show the app in." >&2
exit 1
`, q.Encode(), shellQuote(title), slug)
}

// hostGUIStartupEntries are the .desktop keys that make XFCE show a busy cursor from the click on a Windows app's icon
// until its viewer window appears (see hostGUILauncher for the window's class).
func hostGUIStartupEntries() string {
	host := webAppHost(controlPlaneURL())
	if host == "" {
		return ""
	}
	return "StartupNotify=true\nStartupWMClass=" + host + "__host-gui_view\n"
}

// phoneAppLauncher is the shell that shows an app running on a linked phone: like hostGUILauncher, it opens the control
// plane's viewer in an app window of the VM's own browser, with a token signed for exactly this app on this phone.
func phoneAppLauncher(pc packContext, slug, title, device, pkg string) string {
	id := pc.sessionID("phone-" + slug)
	target := phoneCmdPrefix + device
	q := url.Values{"id": {id}, "desktop": {pc.DesktopID}, "title": {title}, "device": {device}, "package": {pkg}}
	if len(pc.Secret) > 0 {
		q.Set("token", winhost.Token(pc.Secret, id, target, pkg))
	}
	return fmt.Sprintf(`CP="${CONTROL_PLANE_URL:-http://host.docker.internal:8080}"
VIEW="$CP/phone-app/view?%s"
for browser in chromium-browser google-chrome chromium; do
    if command -v "$browser" >/dev/null 2>&1; then
        exec "$browser" --app="$VIEW" --window-size=420,860
    fi
done
echo "No browser is installed in this desktop to show the app in." >&2
exit 1
`, q.Encode())
}

// hostFallback is what a launcher runs after its in-VM options: the hidden host viewer for a Windows app (which must
// never open a window on the host itself), host-run for anything else.
func hostFallback(pc packContext, slug string, app HostAppItem) string {
	if target, args, ok := windowsTarget(app.HostCmd); ok {
		return hostGUILauncher(pc, slug, app.Name, target, args)
	}
	return hostRunFallback(pc, app)
}

// localPageLauncher is the launcher for a Windows app that is only a web app's launcher (localPageFor): its page, opened
// in the VM's own browser, where an agent reads it field by field - the hidden host browser's picture is all a worker
// would get, a demonstration there is bare coordinates, and a password typed into it is not known to be one. The page is
// reached at host.docker.internal; when nothing answers there the app is started on the host as it would be for the
// viewer (its launcher starts the server first: winhost's Prepare) and the page waited for. Only a page the VM still
// cannot reach - a server that listens on the host's loopback alone - is shown through the viewer, as before.
func localPageLauncher(pc packContext, slug, title, target, args, vmPage string) string {
	id := pc.sessionID(slug)
	q := url.Values{"id": {id}, "desktop": {pc.DesktopID}, "target": {target}, "args": {args}}
	if len(pc.Secret) > 0 {
		q.Set("token", winhost.Token(pc.Secret, id, target, args))
	}
	before := fmt.Sprintf(`PAGE=%s
CP="${CONTROL_PLANE_URL:-http://host.docker.internal:8080}"

# Any answer at all is a server: a sign-in page, a redirect, even an error page.
answers() {
    local code
    code=$(curl -s -o /dev/null -m 5 -w '%%{http_code}' "$PAGE" 2>/dev/null)
    [ -n "$code" ] && [ "$code" != 000 ]
}

host_viewer() {
%s}

if ! answers; then
    # Its server is not up: start the app on the host, as the viewer would, which starts the server first.
    curl -s -m 600 -o /dev/null -X POST "$CP/api/host-gui/start" -d %s 2>/dev/null
    for _ in $(seq 1 90); do
        answers && break
        sleep 2
    done
    answers || host_viewer
fi

`, shellQuote(vmPage), hostGUILauncher(pc, slug, title, target, args), shellQuote(q.Encode()))
	return webAppWrapperWith(slug, vmPage, before, hostGUILauncher(pc, slug, title, target, args))
}

// hostRunFallback is host-run of exactly the app's command, signed for this desktop.
func hostRunFallback(pc packContext, app HostAppItem) string {
	// Signed for exactly this command line: the control plane runs it as written and refuses anything else, so a
	// launcher's own arguments ($@) are not passed on.
	words := splitWords(app.HostCmd)
	if len(words) == 0 || len(pc.Secret) == 0 {
		return "echo \"This app cannot be started from here.\" >&2\nexit 1\n"
	}
	token := winhost.ExecToken(pc.Secret, pc.DesktopID, words[0], words[1:])
	return fmt.Sprintf("exec /home/headless/.local/bin/host-run signed %s %s\n", token, shellWords(app.HostCmd))
}

// webAppHost is the window class Chromium gives an --app window for rawURL: the URL's host.
func webAppHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// startupNotifyEntries are the .desktop keys that make XFCE show a busy cursor from the click until the app's window
// (Chromium's class for it is the URL's host) appears, so a slow start does not look like a click that did nothing.
func startupNotifyEntries(webURL string) string {
	host := webAppHost(webURL)
	if host == "" {
		return ""
	}
	return "StartupNotify=true\nStartupWMClass=" + host + "\n"
}

// webAppWrapper is the launcher script for an app that is really a website, shown in the VM's own Chromium in app
// mode. Starting a browser takes seconds on a small host and shows nothing until its window exists, so people clicked
// the icon again and again and got a window for every click. The script therefore raises the app's window if it is
// already open, ignores clicks while a launch is in flight (flock), and keeps the launch in flight until the window
// has appeared. fallback runs only when no browser is installed.
func webAppWrapper(slug, webURL, fallback string) string {
	return webAppWrapperWith(slug, webURL, "", fallback)
}

// webAppWrapperWith is webAppWrapper with before run first, once the display is set.
func webAppWrapperWith(slug, webURL, before, fallback string) string {
	host := webAppHost(webURL)
	return fmt.Sprintf(`#!/usr/bin/env bash
export DISPLAY="${DISPLAY:-:1}"
APP=%s
HOST_RE=%s
%s
app_window() {
    command -v xdotool >/dev/null 2>&1 || return 1
    xdotool search --classname "$HOST_RE" 2>/dev/null | head -1
}

# Already open (possibly minimized): bring it forward instead of opening a second one.
if win=$(app_window) && [ -n "$win" ]; then
    xdotool windowactivate "$win" >/dev/null 2>&1
    exit 0
fi

# A launch is already in flight: this click is a repeat of it.
exec 9>/tmp/decurix-launch-%s.lock
flock -n 9 || exit 0

# First preference: Render directly inside the VM desktop using Chromium standalone app mode.
for browser in chromium-browser google-chrome chromium; do
    if command -v "$browser" >/dev/null 2>&1; then
        # 9>&- : the browser must not inherit the lock, or a cold start would hold it for as long as it runs.
        "$browser" --app="$APP" "$@" >/dev/null 2>&1 9>&- &
        # Hold the lock until the window is up (bounded), so clicks in the meantime are ignored.
        for _ in $(seq 1 900); do
            [ -n "$(app_window)" ] && break
            sleep 0.1
        done
        exit 0
    fi
done
%s`, shellQuote(webURL), shellQuote("^"+regexp.QuoteMeta(host)), before, slug, fallback)
}

// shellQuote single-quotes s for a bash script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func packDynamicApp(tw *tar.Writer, app HostAppItem, pc packContext) error {
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

		if png := windowsAppIcon(app); len(png) > 0 {
			iconTarget := fmt.Sprintf(".local/share/icons/hicolor/48x48/apps/%s.png", slug)
			if err := writeStringToTar(tw, iconTarget, string(png), 0644); err != nil {
				return err
			}
			icon = "/home/headless/" + iconTarget
		} else if svgContent != "" {
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

		// Theme names and inaccessible host paths cannot guarantee an icon in
		// the VM. Package a fallback when no usable image was copied.
		if !strings.HasPrefix(icon, "/home/headless/.local/share/icons/") {
			iconTarget := ".local/share/icons/hicolor/scalable/apps/decurix-app.svg"
			fallback := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect x="3" y="3" width="42" height="42" rx="10" fill="#3b6fb6"/><path fill="white" d="M12 12h10v10H12zm14 0h10v10H26zM12 26h10v10H12zm14 0h10v10H26z"/></svg>`
			if err := writeStringToTar(tw, iconTarget, fallback, 0644); err != nil {
				return err
			}
			icon = "/home/headless/" + iconTarget
		}
		cat := getCategoriesForApp(app.Category)

		var wrapperScript string
		var webURL string
		var startup string // .desktop keys for a busy cursor until the app's window shows
		suffix := "Host"
		if device, pkg, ok := parsePhoneCmd(app.HostCmd); ok {
			// On the phone itself, even when the service also has a web version: that is what was picked.
			wrapperScript = "#!/usr/bin/env bash\n" + phoneAppLauncher(pc, slug, app.Name, device, pkg)
			suffix = "Phone"
		} else if webURL = getWebAppURLForApp(app.Name, app.HostCmd); webURL != "" {
			wrapperScript = webAppWrapper(slug, webURL, hostFallback(pc, slug, app))
			startup = startupNotifyEntries(webURL)
		} else if target, args, ok := windowsTarget(app.HostCmd); ok && vmPageURL(localPageFor(target, args)) != "" {
			vmPage := vmPageURL(localPageFor(target, args))
			wrapperScript = localPageLauncher(pc, slug, app.Name, target, args, vmPage)
			startup = startupNotifyEntries(vmPage)
			// Kept ready on the host as any Windows app is: that is what starts the page's server, ahead of a click.
			startHostAppAhead(pc.DesktopID, pc.sessionID(slug), target, args)
		} else {
			wrapperScript = "#!/usr/bin/env bash\n" + hostFallback(pc, slug, app)
			// This is the exact launcher a host_gui app's icon runs the first time someone opens it - start it now
			// instead of waiting for that click, under the same session id the launcher itself will ask for, so
			// Start finds it already running and joins it at once (see winhost.Manager.Start).
			if target, args, ok := windowsTarget(app.HostCmd); ok {
				startHostAppAhead(pc.DesktopID, pc.sessionID(slug), target, args)
				startup = hostGUIStartupEntries()
			}
		}

		if err := writeStringToTar(tw, ".local/bin/"+slug+"-host", wrapperScript, 0755); err != nil {
			return err
		}

		launcherContent := fmt.Sprintf(`[Desktop Entry]
Version=1.0
Type=Application
Name=%s (%s)
Comment=%s
Exec=/home/headless/.local/bin/%s-host %%U
Icon=%s
Terminal=false
Categories=%s
%s`, app.Name, suffix, app.Description, slug, icon, cat, startup)

		if err := writeHostLauncher(tw, slug, launcherContent); err != nil {
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
# host-run: asks the control plane to open one link or run one command a launcher was written for on the host. It
# cannot do anything else: the control plane only opens a link and runs a command it signed into a launcher.
set -e

CP_URL="${CONTROL_PLANE_URL:-${DECURIX_CONTROL_PLANE:-http://host.docker.internal:8080}}"

# bridge_post <path> <key> <value> [args-json]: POSTs a small JSON object to the control plane and
# prints the response body. python3 rather than curl: the desktop images do not all ship curl, and
# a missing curl made every launcher fail as "host bridge not reachable" without ever sending a request.
# DECURIX_BRIDGE_TOKEN is the launcher's signature for the link or the command.
bridge_post() {
    python3 - "${CP_URL}$1" "$2" "$3" "${4:-}" <<'PY'
import json, os, sys, urllib.request
url, key, value, args = sys.argv[1:5]
body = {key: value, "desktop": os.environ.get("DESKTOP_ID", ""), "token": os.environ.get("DECURIX_BRIDGE_TOKEN", "")}
if args:
    body["args"] = json.loads(args)
    body["display"] = os.environ.get("DISPLAY", ":1")
req = urllib.request.Request(url, json.dumps(body).encode(), {"Content-Type": "application/json"})
sys.stdout.write(urllib.request.urlopen(req, timeout=60).read().decode())
PY
}

usage() {
    cat << EOF
Usage: host-run open <token> <https-link>            (what a copied web app's launcher runs)
       host-run signed <token> <command> [args...]   (what a copied app's launcher runs)

Opens the links and runs the commands Decurix's launchers were written for. Nothing else can be opened or run on the
host from here.
EOF
    exit 0
}

[ $# -eq 0 ] && usage

subcmd="$1"

case "$subcmd" in
    open)
        [ $# -ge 3 ] || usage
        DECURIX_BRIDGE_TOKEN="$2"
        export DECURIX_BRIDGE_TOKEN
        target="$3"
        if bridge_post /api/host-bridge/open target "${target}" >/dev/null 2>&1; then
            echo "Opened on host desktop: ${target}"
            exit 0
        fi
        echo "Error: could not open ${target} on the host (only the link a launcher was written for; is ${CP_URL} reachable?)."
        exit 1
        ;;
    signed)
        [ $# -ge 3 ] || usage
        DECURIX_BRIDGE_TOKEN="$2"
        export DECURIX_BRIDGE_TOKEN
        cmd="$3"
        shift 3
        args_json=$(python3 -c "import json,sys; print(json.dumps(sys.argv[1:]))" "$@")
        response=$(bridge_post /api/host-bridge/exec cmd "$cmd" "$args_json" 2>/dev/null || true)
        if [ -n "$response" ]; then
            python3 -c "import json,sys; res=json.loads(sys.argv[1]); sys.stdout.write(res.get('stdout','')); sys.stderr.write(res.get('stderr','')); sys.exit(res.get('exit_code',0))" "$response"
            exit $?
        fi
        echo "Error: the host bridge at ${CP_URL} did not run '${cmd}'. If this app was copied before host commands were signed, copy it into this desktop again."
        exit 1
        ;;
    -h|--help)
        usage
        ;;
    *)
        echo "Error: host-run cannot run '${subcmd}' on the host. It only opens the links and runs the commands of the launchers Decurix wrote." >&2
        exit 1
        ;;
esac
`

	// No host-open or host-clip any more: a link is opened only for a launcher that was signed for it, and nothing sets
	// the person's clipboard from a desktop (review 2026-11-02).
	return writeStringToTar(tw, ".local/bin/host-run", hostRunScript, 0755)
}

// -----------------------------------------------------------------------------
// Tar Archiving Helpers with Cache Filtering
// -----------------------------------------------------------------------------

// isCacheOrTransient returns true if a directory or file represents disposable caches,
// metrics, or transient crash dumps that cause RAM redundancy and page cache bloat.
func isCacheOrTransient(name, fullPath string, isDir bool) bool {
	lower := strings.ToLower(name)
	// Strictly exclude self / app repository to prevent recursive nesting
	// by the directory being visited. An ancestor with "decurix" in its name
	// does not make every cookie and settings file below it a repository.
	if isDir && (lower == "decurix" || lower == "migration-toolkit") {
		return true
	}

	// Browser and toolchain caches
	if strings.Contains(lower, "cache") {
		return true
	}
	if strings.HasPrefix(lower, "cached") {
		return true
	}
	if lower == "gpu-cache" || lower == "gpucache" || lower == "code cache" || lower == "codecache" || lower == "dawncache" || lower == "grshadercache" {
		return true
	}
	if lower == "crashpad" || lower == "crash reports" || strings.HasPrefix(lower, "browsermetrics") || lower == "minidumps" {
		return true
	}
	if strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".dmp") || strings.HasSuffix(lower, ".swp") {
		return true
	}
	if isDir && (lower == "logs" || lower == "node_modules" || lower == "pkg" || lower == "temp" ||
		lower == "indexeddb" || lower == "service worker" || lower == "blob_storage" || lower == "file system" ||
		lower == "storage" || lower == "extensions" || lower == "dictionaries" || lower == "media cache" ||
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

// WindowsCDrives returns where this process can read the host's C: drive (a WSL or container mount, or C:\ itself).
func WindowsCDrives() []string {
	return getWindowsCDriveCandidates()
}
