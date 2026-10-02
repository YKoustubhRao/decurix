package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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

// builtInApps are the shortcuts every native worker has, ahead of the ones
// picked for it: the browser, as a Linux desktop has (its "Google Chrome"
// icon and its panel's pinned browser). It goes through the agent's /open by
// name, so it is the worker's own browser, with its DevTools port.
var builtInApps = []NativeApp{
	{ID: "decurix:browser", Name: "Microsoft Edge", Kind: nativeAppNamed, Target: "browser"},
}

// nativeAppNamed is a built-in: something the worker's agent opens by name.
const nativeAppNamed = "named"

// builtInIcons is what Windows is asked for each built-in's picture.
var builtInIcons = map[string]string{
	"decurix:browser": "msedge.exe",
}

// allApps is every shortcut a worker has: the built-ins, then its own.
func allApps(meta nativeMeta) []NativeApp {
	apps := append([]NativeApp{}, builtInApps...)
	return append(apps, meta.Apps...)
}

// shortcuts is a worker's apps as the icons the helper draws.
func shortcuts(meta nativeMeta) []winhost.Shortcut {
	var out []winhost.Shortcut
	for _, app := range allApps(meta) {
		icon := builtInIcons[app.ID]
		if app.Kind == NativeAppProgram {
			icon = app.Target
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
		appID, isOpen := strings.CutPrefix(event, "open ")
		if !isOpen || h.open == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), nativeOpenTimeout)
		defer cancel()

		if _, err := h.open(ctx, id, strings.TrimSpace(appID)); err != nil {
			log.Printf("worker %s: opening %s from its desktop: %v", id, appID, err)
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

// NativeShortcuts are the shortcuts on a native worker's screen: the
// built-ins, then the apps picked for it.
func (m *Manager) NativeShortcuts(rawID string) ([]NativeApp, error) {
	if m.native == nil {
		return nil, errors.New("this control plane's desktops are containers")
	}

	meta, err := m.native.readMeta(sanitizeName(rawID))
	if err != nil {
		return nil, err
	}

	return allApps(meta), nil
}

// OpenNativeApp opens one of a worker's shortcuts on its screen and says what
// happened. An address, or a built-in, goes the way the Start menu's box does
// - through the agent, so it is a step of a demonstration being recorded; a
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

	if app.Kind == NativeAppProgram {
		if err := m.OpenNativeProgram(rawID, *app); err != nil {
			if errors.Is(err, ErrNotRunning) {
				return "", err
			}
			return "", &NativeOpenRefused{Reason: app.Name + " could not be opened: " + err.Error()}
		}

		return "Opened " + app.Name + ".", nil
	}

	body, err := json.Marshal(map[string]string{"target": app.Target})
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
			answer.Summary = "Opened " + app.Name + "."
		}
		return answer.Summary, nil
	case resp.StatusCode < 500 && answer.Error != "":
		// The agent's own words for a person ("don't know how to open ...").
		return "", &NativeOpenRefused{Reason: answer.Error}
	default:
		return "", fmt.Errorf("the worker's agent answered %d to opening %s: %s", resp.StatusCode, app.ID, bytes.TrimSpace(raw))
	}
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
