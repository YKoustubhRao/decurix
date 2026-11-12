// Per-app "bring my sign-ins" for the host apps given to a desktop: chosen at the person's own screen, per app.
package desktop

import (
	"strings"
	"sync"
)

// ============================================================
// "Bring my sign-ins", per app
//
// A browser copied into a desktop runs on this computer with a private profile of its own, and that profile starts
// empty: the person's cookies, saved passwords and site logins (WhatsApp Web keeps its login in the profile's
// IndexedDB) in a browser a worker drives could be used by anything that steers the worker (review 2026-11-02). Some
// people want exactly that for an app they pick, so it is their choice, made for each app when they give it to a
// desktop, on this computer's own screen (the create and copy routes refuse host apps on the gated listener).
//
// It used to be one machine-wide switch in the control plane's .env (DECURIX_IMPORT_HOST_BROWSER_PROFILE, still
// honoured). A fresh rebuild deletes that file, and the next launch then deleted the imported copy and opened an empty
// browser (found 2026-11-12). The choice is now kept by the control plane with the app's host session id (told the
// Windows helper before every launch, winhost.Manager.SetSignIns), never in the launcher the worker can read.
// ============================================================

var (
	hostAppSignInsMu sync.RWMutex
	hostAppSignIns   func(sessionID string, on bool)
)

// SetHostAppSignIns installs what noteHostAppSignIns calls: the control plane keeps, per host session, whether it brings
// the person's sign-ins, and says so to the Windows helper when it launches the app.
func SetHostAppSignIns(f func(sessionID string, on bool)) {
	hostAppSignInsMu.Lock()
	hostAppSignIns = f
	hostAppSignInsMu.Unlock()
}

// noteHostAppSignIns tells the installed hook; told before the app may be started ahead (startHostAppAhead), so that
// first start already has the person's choice.
func noteHostAppSignIns(sessionID string, on bool) {
	hostAppSignInsMu.RLock()
	f := hostAppSignIns
	hostAppSignInsMu.RUnlock()
	if f != nil {
		f(sessionID, on)
	}
}

// SetSignInApps records, for the host apps being given to a desktop (requested), which bring the person's sign-ins
// (ticked: only those that were also requested count). The choice for any other app already on the desktop is kept, so
// copying one more app later never changes an earlier one's. Call it before the copy; a retry of apps the host could
// not be asked about yet reads it again, so the choice survives the wait.
func (m *Manager) SetSignInApps(desktopID string, requested, ticked []string) {
	ask := map[string]bool{}
	for _, id := range requested {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			ask[id] = true
		}
	}
	m.signInMu.Lock()
	defer m.signInMu.Unlock()
	if m.signIns == nil {
		m.signIns = map[string]map[string]bool{}
	}
	key := sanitizeName(desktopID)
	set := m.signIns[key]
	if set == nil {
		set = map[string]bool{}
	}
	for id := range ask {
		delete(set, id)
	}
	for _, id := range ticked {
		if id = strings.ToLower(strings.TrimSpace(id)); ask[id] {
			set[id] = true
		}
	}
	if len(set) == 0 {
		delete(m.signIns, key)
		return
	}
	m.signIns[key] = set
}

// SignInApps is a copy of what SetSignInApps recorded for the desktop.
func (m *Manager) SignInApps(desktopID string) map[string]bool { return m.signInAppsOf(desktopID) }

// ReplaceSignInApps puts back what SignInApps returned: for a creation that was refused after its choice was recorded.
func (m *Manager) ReplaceSignInApps(desktopID string, set map[string]bool) {
	m.signInMu.Lock()
	defer m.signInMu.Unlock()
	if m.signIns == nil {
		m.signIns = map[string]map[string]bool{}
	}
	if len(set) == 0 {
		delete(m.signIns, sanitizeName(desktopID))
		return
	}
	m.signIns[sanitizeName(desktopID)] = set
}

// signInAppsOf is a copy of what SetSignInApps recorded for the desktop.
func (m *Manager) signInAppsOf(desktopID string) map[string]bool {
	m.signInMu.Lock()
	defer m.signInMu.Unlock()
	out := map[string]bool{}
	for id := range m.signIns[sanitizeName(desktopID)] {
		out[id] = true
	}
	return out
}

// forgetSignInApps drops the choices of a deleted desktop.
func (m *Manager) forgetSignInApps(desktopID string) {
	m.signInMu.Lock()
	delete(m.signIns, sanitizeName(desktopID))
	m.signInMu.Unlock()
}

// bringsSignIns is whether app, packed with pc, brings the person's sign-ins: ticked for this desktop, and a browser
// (the only kind whose private profile the bridge can seed from the person's own).
func bringsSignIns(pc packContext, app HostAppItem) bool {
	return pc.SignIns[strings.ToLower(app.ID)] && isBrowserApp(app.Name)
}
