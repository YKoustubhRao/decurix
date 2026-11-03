package desktop

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// A desktop's agent names itself (desktop_id) in what it sends the control plane, and every desktop can reach the
// control plane, so a name in a body proves nothing: any desktop could claim to be any other and read its memory,
// answer its questions or post its outcomes (review 2026-11-02). The address a request comes from is Docker's to
// assign, so DesktopAt says which desktop that address belongs to.

// callerTTL is how long the address map is trusted; callerRefreshEvery bounds how often an unknown address makes
// it refresh (a desktop just started). A desktop made or removed through the control plane drops the map at once
// (the desktop list's generation), so an address handed to a new desktop is not read as the old one's.
const (
	callerTTL          = 15 * time.Second
	callerRefreshEvery = 2 * time.Second
)

// callerCache is the address map and the network view, each fetched by one call at a time (refreshed).
type callerCache struct {
	byAddr refreshed[map[netip.Addr]string]
	nets   netCache

	// list and netsFetch stand in for Docker in tests.
	list      func(ctx context.Context) (map[netip.Addr]string, error)
	netsFetch func(ctx context.Context) (NetView, bool)
}

// DesktopAt returns the ID of the desktop whose container has addr on the desktop network, and false when no
// desktop does (or Docker cannot be asked). A known address in a fresh map is answered without waiting; only an
// address the map does not hold, or a map past callerTTL, waits for Docker, and every caller waiting then shares one
// question.
func (m *Manager) DesktopAt(ctx context.Context, addr netip.Addr) (string, bool) {
	if m == nil || (m.client == nil && m.callers.list == nil) || !addr.IsValid() {
		return "", false
	}
	addr = addr.Unmap()
	gen := m.desktopGeneration()

	byAddr, ok := m.callers.byAddr.fresh(ctx, callerTTL, gen, m.fetchCallers)
	if !ok {
		return "", false
	}
	if id, found := byAddr[addr]; found {
		return id, true
	}
	// Not in the map: a desktop started a moment ago, or no desktop at all. Asked again at most every
	// callerRefreshEvery, so a container that is no desktop cannot make every request wait for Docker.
	byAddr, ok = m.callers.byAddr.fresh(ctx, callerRefreshEvery, gen, m.fetchCallers)
	if !ok {
		return "", false
	}
	id, found := byAddr[addr]
	return id, found
}

// desktopGeneration is the desktop list's generation, bumped by every desktop made or removed here.
func (m *Manager) desktopGeneration() uint64 {
	m.listMu.Lock()
	defer m.listMu.Unlock()
	return m.listGen
}

// fetchCallers asks Docker which desktop has which address on the desktop network. Unknown is unknown: when Docker
// cannot be asked the map is empty, since an old map is not evidence about who is calling now.
func (m *Manager) fetchCallers(ctx context.Context, _ map[netip.Addr]string) map[netip.Addr]string {
	if m.callers.list != nil {
		byAddr, err := m.callers.list(ctx)
		if err != nil {
			return map[netip.Addr]string{}
		}
		return byAddr
	}
	list, err := m.client.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "desktop-manager=true"))})
	if err != nil {
		return map[netip.Addr]string{}
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
	return byAddr
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
