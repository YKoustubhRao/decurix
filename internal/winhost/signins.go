// Per-app "bring my sign-ins": the choice a person made for one app, kept with its host session.
package winhost

// A browser copied into a desktop runs on this computer with a private profile of its own, empty unless the person chose
// to bring the sign-ins of their own browser along for that app (the bridge's Initialize-PrivateBrowserProfile). The
// choice is theirs, made at their own screen when they give the app to a desktop; the control plane keeps it with the
// app's session id and says it to the helper before every launch (`signins 1|0`), so a helper reused for another app
// never carries the last one's answer, and nothing in the VM can change it.

// SetSignIns records whether session id brings the person's sign-ins; it applies from the next launch of that session.
func (m *Manager) SetSignIns(id string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.signIns == nil {
		m.signIns = make(map[string]bool)
	}
	if on {
		m.signIns[id] = true
	} else {
		delete(m.signIns, id)
	}
}

// signInsOf is whether session id brings the person's sign-ins.
func (m *Manager) signInsOf(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.signIns[id]
}

// signInsLine is the helper command that says it.
func signInsLine(on bool) string {
	if on {
		return "signins 1"
	}
	return "signins 0"
}
