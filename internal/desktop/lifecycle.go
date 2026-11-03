package desktop

import "sync"

// LockDesktop takes desktop id's lifecycle lock and returns its release. Whatever decides from a desktop's state and
// then acts on it - delivering a task, an idle pause or stop, a start, stop, pause or delete - holds it from the check
// to the act, so an idle sweep can never freeze a desktop a task was just delivered to (B-23). The Manager's own
// methods do not take it (a caller holding it calls them); it is the callers' to take, around the check and the act.
func (m *Manager) LockDesktop(id string) (unlock func()) {
	key := sanitizeName(id)
	m.lifecycleMu.Lock()
	if m.lifecycle == nil {
		m.lifecycle = map[string]*sync.Mutex{}
	}
	mu, ok := m.lifecycle[key]
	if !ok {
		mu = &sync.Mutex{}
		m.lifecycle[key] = mu
	}
	m.lifecycleMu.Unlock()
	mu.Lock()
	return mu.Unlock
}
