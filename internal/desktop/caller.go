package desktop

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// A desktop's agent names itself (desktop_id) in what it sends the control plane, and every desktop can reach the
// control plane, so a name in a body proves nothing: any desktop could claim to be any other and read its memory,
// answer its questions or post its outcomes (review 2026-11-02). The address a request comes from is Docker's to
// assign, so DesktopAt says which desktop that address belongs to.

// callerTTL is how long the address map is trusted; callerRefreshEvery bounds how often an unknown address makes
// it refresh (a desktop just started).
const (
	callerTTL          = 15 * time.Second
	callerRefreshEvery = 2 * time.Second
)

type callerCache struct {
	mu      sync.Mutex
	byAddr  map[netip.Addr]string
	fetched time.Time
}

// DesktopAt returns the ID of the desktop whose container has addr on the desktop network, and false when no
// desktop does (or Docker cannot be asked).
func (m *Manager) DesktopAt(ctx context.Context, addr netip.Addr) (string, bool) {
	if m == nil || m.client == nil || !addr.IsValid() {
		return "", false
	}
	addr = addr.Unmap()

	m.callers.mu.Lock()
	defer m.callers.mu.Unlock()

	age := time.Since(m.callers.fetched)
	if id, ok := m.callers.byAddr[addr]; ok && age < callerTTL {
		return id, true
	}
	if age < callerRefreshEvery && m.callers.byAddr != nil {
		return "", false
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	list, err := m.client.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "desktop-manager=true"))})
	m.callers.fetched = time.Now()
	if err != nil {
		// Unknown is unknown: an old map is not evidence about who is calling now.
		m.callers.byAddr = map[netip.Addr]string{}
		return "", false
	}
	byAddr := make(map[netip.Addr]string, len(list))
	for _, c := range list {
		id := desktopIDFromNames(c.Names)
		if id == "" || c.NetworkSettings == nil {
			continue
		}
		if n := c.NetworkSettings.Networks[NetworkName]; n != nil {
			if a, err := netip.ParseAddr(n.IPAddress); err == nil {
				byAddr[a.Unmap()] = id
			}
		}
	}
	m.callers.byAddr = byAddr
	id, ok := byAddr[addr]
	return id, ok
}

// desktopIDFromNames is the desktop ID in a container's names ("/desktop_<id>"), "" for any other container.
func desktopIDFromNames(names []string) string {
	for _, n := range names {
		n = strings.TrimPrefix(n, "/")
		if id, ok := strings.CutPrefix(n, "desktop_"); ok && id != "" {
			return id
		}
	}
	return ""
}
