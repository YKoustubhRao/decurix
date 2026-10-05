package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// A native worker's screen is a desktop like any other to the person looking
// at it: a wallpaper with icons on it, windows that come to the front when
// clicked and move by their title bars, and a taskbar. The private desktop it
// really is has none of that (no Explorer runs there), so the pieces are
// supplied: winhost's helper paints the wallpaper and icons and does the
// window manager's work, the screen's page draws the taskbar and Start menu,
// and this file is what they both ask - which shortcuts the worker has, what
// one opens, which windows are open.

// A worker has the shortcuts picked for it and no others: no browser is put
// on its desktop or taskbar by default.

// browserKinds are the browsers a worker can have as its own, by the name the
// host-app scan gives one and by the last part of what starts it (a program,
// or a Start-menu address such as shell:AppsFolder\Chrome). The kinds are the
// agent's (computer/browser.py's _WINDOWS_KINDS).
var browserKinds = []struct {
	kind  string
	names []string
	bases []string
}{
	{"chrome", []string{"google chrome", "chrome"}, []string{"chrome.exe", "chrome"}},
	{"brave", []string{"brave", "brave browser"}, []string{"brave.exe", "brave"}},
	{"msedge", []string{"microsoft edge", "edge"}, []string{"msedge.exe", "msedge"}},
}

// pickedBrowser is what a shortcut that is a browser says about it.
type pickedBrowser struct {
	kind string
	// exe is the program, when the shortcut names it; a Start-menu address
	// names none, and the agent finds the browser where it installs.
	exe string
	// profile is the browser profile the shortcut opens ("Profile 2"), or "".
	profile string
}

// profileArgRe is the one argument a shortcut to the browser itself may
// carry: which of the browser's profiles to open.
var profileArgRe = regexp.MustCompile(`(?i)^--profile-directory=(?:"([^"]*)"|(\S+))$`)

// browserOf is which browser a shortcut is; ok is false for anything else.
// A shortcut file is read for what it starts: found live, the browser picked
// for a worker was "Koustubh - Chrome.lnk" - the shortcut Chrome makes for a
// profile - which is Chrome by neither its name nor its file name, so the
// worker was taken to have no browser and its agent opened Edge. A browser
// started with any argument but its profile is a page of it (an installed
// web app), not the browser.
func browserOf(app NativeApp) (b pickedBrowser, ok bool) {
	if app.Kind != NativeAppProgram {
		return b, false
	}

	target, args := app.Target, strings.TrimSpace(app.Args)
	if strings.HasSuffix(strings.ToLower(target), ".lnk") {
		if t, a, read := readShortcut(target); read {
			target = t
			if args == "" { // as the helper starts it: the shortcut's own arguments
				args = strings.TrimSpace(a)
			}
		}
	}

	if args != "" {
		m := profileArgRe.FindStringSubmatch(args)
		if m == nil {
			return b, false
		}
		b.profile = m[1] + m[2]
	}

	base := strings.ToLower(target)
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	name := strings.ToLower(strings.TrimSpace(app.Name))

	for _, k := range browserKinds {
		for _, n := range k.names {
			if name == n {
				b.kind = k.kind
			}
		}
		for _, n := range k.bases {
			if base == n {
				b.kind = k.kind
			}
		}
		if b.kind != "" {
			break
		}
	}
	if b.kind == "" {
		return b, false
	}

	if strings.HasSuffix(strings.ToLower(target), ".exe") {
		b.exe = target
	}

	return b, true
}

// browserKind is which browser a shortcut is, or "" for anything else.
func browserKind(app NativeApp) string {
	b, _ := browserOf(app)
	return b.kind
}

// workerBrowser is the browser picked for a worker: the first of its
// shortcuts that is one. It is the worker's only browser - the one its icon
// opens and the one its agent opens pages in. Found live: a person copied
// Chrome to a worker and used it, and the agent opened its pages in Edge.
func workerBrowser(apps []NativeApp) (app NativeApp, b pickedBrowser, ok bool) {
	for _, a := range apps {
		if b, ok := browserOf(a); ok {
			return a, b, true
		}
	}

	return NativeApp{}, pickedBrowser{}, false
}

// browserChoiceFile is where a worker's agent reads which browser is its own
// (computer/browser.py's CHOICE_FILE), under the worker's .decurix.
const browserChoiceFile = "worker-browser.json"

// noteBrowser tells the worker's agent which browser was picked for it, or
// that none was. The agent reads it at every launch, so a browser added to a
// running worker counts at once.
func (h *nativeHost) noteBrowser(meta nativeMeta) {
	file := filepath.Join(h.home(meta.ID), ".decurix", browserChoiceFile)

	app, b, ok := workerBrowser(meta.Apps)
	if !ok {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("worker %s: %v", meta.ID, err)
		}
		return
	}

	choice := map[string]string{"kind": b.kind}
	if b.exe != "" {
		choice["exe"] = b.exe
	}
	if b.profile != "" {
		choice["profile_directory"] = b.profile
	}

	// A browser opened from this worker's icon before the agent was the one
	// to open it was started by the helper, with a profile the helper keeps
	// for the worker - and whatever the person signed in to on the worker's
	// screen is in that profile. The agent goes on with it: found live, a
	// person signed in to a site in the worker's Chrome as the agent asked,
	// and the agent then looked for the sign-in in a browser of its own.
	if dir := winhost.ProfileDir(nativeSessionID(meta.ID), b.kind); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			choice["user_data_dir"] = dir
		}
	}

	data, err := json.Marshal(choice)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(file), 0o755); err == nil {
			err = os.WriteFile(file, data, 0o644)
		}
	}
	if err != nil {
		log.Printf("worker %s: its agent was not told its browser is %s: %v", meta.ID, app.Name, err)
	}
}

// workerAppsFile is where a worker's agent reads which shortcuts its screen
// has (computer/shortcuts.py's APPS_FILE), under the worker's .decurix.
const workerAppsFile = "worker-apps.json"

// agentApp is one of a worker's shortcuts as its agent is told of it.
type agentApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is "browser" (the worker's own browser), "web" (an address, in
	// Target) or "program".
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
	// Exe is the program a shortcut starts, when the shortcut itself says:
	// how the agent tells which shortcut an open window belongs to.
	Exe string `json:"exe,omitempty"`
}

// noteApps tells the worker's agent which shortcuts its screen has. The
// agent opens one by its name, the way a person opens one by its icon - so
// what a person opened while recording, a replay opens again - and it is how
// a replay knows what to open when a step's window is not there.
func (h *nativeHost) noteApps(meta nativeMeta) {
	h.noteBrowser(meta)

	_, browser, hasBrowser := workerBrowser(meta.Apps)
	seenBrowser := false

	apps := []agentApp{}
	for _, app := range meta.Apps {
		told := agentApp{ID: app.ID, Name: app.Name, Kind: app.Kind}

		switch {
		case app.Kind == NativeAppWeb:
			told.Target = app.Target
		case hasBrowser && !seenBrowser && browserKind(app) == browser.kind:
			told.Kind, told.Exe, seenBrowser = "browser", browser.exe, true
		default:
			target := app.Target
			if strings.HasSuffix(strings.ToLower(target), ".lnk") {
				if resolved, _, ok := readShortcut(target); ok {
					target = resolved
				}
			}
			if strings.HasSuffix(strings.ToLower(target), ".exe") {
				told.Exe = target
			}
		}

		apps = append(apps, told)
	}

	file := filepath.Join(h.home(meta.ID), ".decurix", workerAppsFile)
	data, err := json.Marshal(apps)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(file), 0o755); err == nil {
			err = os.WriteFile(file, data, 0o644)
		}
	}
	if err != nil {
		log.Printf("worker %s: its agent was not told its shortcuts: %v", meta.ID, err)
	}
}

// shortcuts is a worker's apps as the icons the helper draws.
func shortcuts(meta nativeMeta) []winhost.Shortcut {
	var out []winhost.Shortcut
	for _, app := range meta.Apps {
		icon := ""
		if app.Kind == NativeAppProgram {
			icon = app.Target
		}
		if app.Icon != "" {
			icon = app.Icon
		}
		out = append(out, winhost.Shortcut{ID: app.ID, Name: app.Name, Icon: icon})
	}

	return out
}

// dress gives a worker's new desktop its wallpaper and icons, and listens for
// a double-click on one. Best-effort: a desktop that could not be dressed is
// still a worker.
func (h *nativeHost) dress(id string, session *winhost.Session, meta nativeMeta) {
	session.OnEvent(func(event string) {
		ctx, cancel := context.WithTimeout(context.Background(), nativeOpenTimeout)
		defer cancel()

		if appID, isOpen := strings.CutPrefix(event, "open "); isOpen && h.open != nil {
			if _, err := h.open(ctx, id, strings.TrimSpace(appID)); err != nil {
				log.Printf("worker %s: opening %s from its desktop: %v", id, appID, err)
			}
		}

		// The Start panel's search, asked to look its words up.
		if words, isWeb := strings.CutPrefix(event, "web "); isWeb && h.web != nil {
			if _, err := h.web(ctx, id, webTarget(words)); err != nil {
				log.Printf("worker %s: looking something up from its Start panel: %v", id, err)
			}
		}
	})

	if err := session.SetShortcuts(meta.Name, shortcuts(meta)); err != nil {
		log.Printf("worker %s: its desktop has no icons: %v", id, err)
	}
}

// redress redraws a running worker's icons after its shortcuts changed.
func (h *nativeHost) redress(id string) {
	meta, err := h.readMeta(id)
	if err != nil {
		return
	}

	if r := h.run(id); r != nil {
		if err := r.session.SetShortcuts(meta.Name, shortcuts(meta)); err != nil {
			log.Printf("worker %s: %v", id, err)
		}
	}
}

// How long opening a shortcut may take: a browser starting cold, or one
// importing a profile the first time it runs for this worker.
const nativeOpenTimeout = 3 * time.Minute

// NativeOpenRefused is why a shortcut could not be opened, in words written
// for the person who asked: the agent's or the helper's own sentence.
type NativeOpenRefused struct{ Reason string }

func (e *NativeOpenRefused) Error() string { return e.Reason }

// NativeShortcuts are the shortcuts on a native worker's screen: the apps
// picked for it.
func (m *Manager) NativeShortcuts(rawID string) ([]NativeApp, error) {
	if m.native == nil {
		return nil, errors.New("this control plane's desktops are containers")
	}

	meta, err := m.native.readMeta(sanitizeName(rawID))
	if err != nil {
		return nil, err
	}

	return meta.Apps, nil
}

// OpenNativeApp opens one of a worker's shortcuts on its screen and says what
// happened. An address, or the worker's browser, goes the way the Start
// menu's box does - through the agent, so it is a step of a demonstration
// being recorded; any other
// program is started on the worker's desktop by the helper that holds it,
// which knows how to keep it off the person's own. ErrNotFound for an id the
// worker does not have: this opens what was picked for it, never a path.
func (m *Manager) OpenNativeApp(ctx context.Context, rawID, appID string) (string, error) {
	apps, err := m.NativeShortcuts(rawID)
	if err != nil {
		return "", err
	}

	var app *NativeApp
	for i := range apps {
		if apps[i].ID == appID {
			app = &apps[i]
		}
	}
	if app == nil {
		return "", fmt.Errorf("shortcut %q: %w", appID, ErrNotFound)
	}

	// The worker's browser is opened by its agent, like an address: started
	// by the helper it would have no DevTools port, and the agent would not
	// be able to read its pages - or would start another browser beside it.
	target := app.Target
	b, _, hasBrowser := workerBrowser(apps)
	isBrowser := hasBrowser && b.ID == app.ID
	if isBrowser {
		target = "browser"
	}

	if app.Kind == NativeAppProgram && !isBrowser {
		// By its name, through the agent - which has this control plane's
		// helper start it (OpenWorkerProgram) and waits for its window. So a
		// program opened from an icon or the Start menu is a step of a
		// demonstration being recorded, as an address or the browser is, and
		// a replay opens it by the same road. Found live: Character Map
		// opened from the Start menu while recording left no trace in the
		// recording, and its replay began by clicking in a window nothing
		// had opened.
		summary, err := m.agentOpen(ctx, rawID, app.Name, app.Name)
		if err == nil {
			return summary, nil
		}

		var refused *NativeOpenRefused
		if errors.As(err, &refused) || errors.Is(err, ErrNotRunning) {
			return "", err
		}

		// The agent could not be asked (it is still starting): the program
		// is opened all the same, as it was before the agent was the way.
		log.Printf("worker %s: opening %s without its agent: %v", sanitizeName(rawID), app.Name, err)
		if err := m.openProgram(rawID, *app); err != nil {
			return "", err
		}

		return "Opened " + app.Name + ".", nil
	}

	return m.agentOpen(ctx, rawID, target, app.Name)
}

// openProgram has the helper start a program shortcut, with why not in words
// for the person when it cannot.
func (m *Manager) openProgram(rawID string, app NativeApp) error {
	if err := m.OpenNativeProgram(rawID, app); err != nil {
		if errors.Is(err, ErrNotRunning) {
			return err
		}
		return &NativeOpenRefused{Reason: app.Name + " could not be opened: " + err.Error()}
	}

	return nil
}

// OpenWorkerProgram starts one of a worker's program shortcuts on its screen
// for the worker's own agent, which asks by the shortcut's id (and then waits
// for the window itself). Only a shortcut picked for that worker, and never
// its browser, which the agent starts itself: ErrNotFound for anything else.
func (m *Manager) OpenWorkerProgram(rawID, appID string) error {
	apps, err := m.NativeShortcuts(rawID)
	if err != nil {
		return err
	}

	b, _, hasBrowser := workerBrowser(apps)

	for _, app := range apps {
		if app.ID != appID {
			continue
		}
		if app.Kind != NativeAppProgram || (hasBrowser && b.ID == app.ID) {
			break
		}
		return m.openProgram(rawID, app)
	}

	return fmt.Errorf("program shortcut %q: %w", appID, ErrNotFound)
}

// agentOpen has a worker's agent open a page, a file or its browser on the
// worker's screen, as a person's own doing (a step of a demonstration being
// recorded), and says what happened. name is what a person calls the thing.
func (m *Manager) agentOpen(ctx context.Context, rawID, target, name string) (string, error) {
	body, err := json.Marshal(map[string]string{"target": target})
	if err != nil {
		return "", err
	}

	resp, err := m.AgentRequest(ctx, rawID, http.MethodPost, "/open", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var answer struct {
		Summary string `json:"summary"`
		Error   string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	_ = json.Unmarshal(raw, &answer)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if answer.Summary == "" {
			answer.Summary = "Opened " + name + "."
		}
		return answer.Summary, nil
	case resp.StatusCode < 500 && answer.Error != "":
		// The agent's own words for a person ("don't know how to open ...").
		return "", &NativeOpenRefused{Reason: answer.Error}
	default:
		return "", fmt.Errorf("the worker's agent answered %d to opening %s: %s", resp.StatusCode, name, bytes.TrimSpace(raw))
	}
}

// addressRe is something typed that is already a web address: a link, or a
// host name with a dot in it (or localhost), with or without a path.
var addressRe = regexp.MustCompile(`(?i)^(https?://\S+|([a-z0-9-]+\.)+[a-z]{2,}(:\d+)?(/\S*)?|localhost(:\d+)?(/\S*)?)$`)

// webTarget is what the Start panel's search opens for the words typed into
// it: the address when they are one, else a web search for them.
func webTarget(words string) string {
	words = strings.TrimSpace(words)
	if addressRe.MatchString(words) {
		return words
	}

	return "https://www.google.com/search?q=" + url.QueryEscape(words)
}

// NativeWindows lists the windows open on a native worker's screen, as a
// taskbar would: the one in front first. None for a worker that is not running.
func (m *Manager) NativeWindows(rawID string) []winhost.TaskWindow {
	if m.native == nil {
		return nil
	}

	session, err := m.native.session(sanitizeName(rawID))
	if err != nil {
		return nil
	}

	windows, err := session.Tasks()
	if err != nil {
		return nil
	}

	return windows
}

// NativeWindowDo does what a taskbar button does to one of a worker's
// windows: "raise", "min" or "close".
func (m *Manager) NativeWindowDo(rawID, action string, window int64) error {
	session, err := m.NativeSession(rawID)
	if err != nil {
		return err
	}

	return session.Window(action, window)
}
