package desktop

import (
	"context"
	"errors"
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
	byAddr refreshed[callerMap]
	nets   netCache

	// list and netsFetch stand in for Docker in tests.
	list      func(ctx context.Context) (map[netip.Addr]string, error)
	netsFetch func(ctx context.Context) (NetView, bool)
}

// DesktopAt returns the ID of the desktop whose container has addr on the desktop network, and false when no
// desktop does (or Docker cannot be asked). CallerAt tells those two apart.
func (m *Manager) DesktopAt(ctx context.Context, addr netip.Addr) (string, bool) {
	id, found, _ := m.CallerAt(ctx, addr)
	return id, found
}

// ErrCallersUnread is CallerAt's answer when Docker could not be asked who has an address and the last map it gave
// is too old to say: not "this is no desktop" (a stranger, refused for good) but "try again" (found live 2026-11-11: a
// Docker blip under load answered a desktop's model call "this address is not a desktop's", and the task ended on it).
var ErrCallersUnread = errors.New("the desktop list could not be read")

// callerMap is what the last question to Docker gave. read is false when Docker could not be asked; byAddr, at and
// gen are then still the last map Docker did give, used only while it is as young as a map is ever trusted
// (callerTTL) - never past that, and never for an address it did not hold.
type callerMap struct {
	byAddr map[netip.Addr]string
	read   bool
	at     time.Time // when Docker gave byAddr
	gen    uint64    // the desktop list's generation byAddr was read for
}

// known is the desktop at addr in a map Docker gave for generation gen (or later) less than callerTTL ago.
func (c callerMap) known(addr netip.Addr, gen uint64) (string, bool) {
	if c.byAddr == nil || c.gen < gen || time.Since(c.at) >= callerTTL {
		return "", false
	}
	id, ok := c.byAddr[addr]
	return id, ok
}

// CallerAt returns the ID of the desktop whose container has addr on the desktop network. found false with a nil
// error is Docker's answer that no desktop has addr; ErrCallersUnread is that Docker could not be asked and the last
// map it gave (younger than callerTTL, of this desktop list) does not name addr. A known address in a fresh map is
// answered without waiting; only an address the map does not hold, or a map past callerTTL, waits for Docker, and
// every caller waiting then shares one question.
func (m *Manager) CallerAt(ctx context.Context, addr netip.Addr) (string, bool, error) {
	if m == nil || (m.client == nil && m.callers.list == nil) || !addr.IsValid() {
		return "", false, nil
	}
	addr = addr.Unmap()
	gen := m.desktopGeneration()
	fetch := func(ctx context.Context, prev callerMap) callerMap { return m.fetchCallers(ctx, prev, gen) }

	if cm, ok := m.callers.byAddr.fresh(ctx, callerTTL, gen, fetch); ok {
		if id, found := cm.known(addr, gen); found {
			return id, true, nil
		}
	}
	// Not in the map: a desktop started a moment ago, or no desktop at all (or the last question failed). Asked again
	// at most every callerRefreshEvery, so a container that is no desktop cannot make every request wait for Docker.
	cm, ok := m.callers.byAddr.fresh(ctx, callerRefreshEvery, gen, fetch)
	if !ok {
		return "", false, ErrCallersUnread
	}
	if id, found := cm.known(addr, gen); found {
		return id, true, nil
	}
	if !cm.read {
		return "", false, ErrCallersUnread
	}
	return "", false, nil
}

// desktopGeneration is the desktop list's generation, bumped by every desktop made or removed here.
func (m *Manager) desktopGeneration() uint64 {
	m.listMu.Lock()
	defer m.listMu.Unlock()
	return m.listGen
}

// fetchCallers asks Docker which desktop has which address on the desktop network, for the desktop list's generation
// gen. Unknown is unknown: when Docker cannot be asked the answer is unread (read false), carrying the last map Docker
// gave with its own time and generation, so no failure makes an old map look new (callerMap.known bounds its use).
func (m *Manager) fetchCallers(ctx context.Context, prev callerMap, gen uint64) callerMap {
	failed := callerMap{byAddr: prev.byAddr, at: prev.at, gen: prev.gen}
	if m.callers.list != nil {
		byAddr, err := m.callers.list(ctx)
		if err != nil {
			return failed
		}
		return callerMap{byAddr: byAddr, read: true, at: time.Now(), gen: gen}
	}
	list, err := m.client.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "desktop-manager=true"))})
	if err != nil {
		return failed
	}
	byAddr := make(map[netip.Addr]string, len(list))
	for _, c := range list {
		id := desktopIDFromNames(c.Names)
		if id == "" || c.NetworkSettings == nil {
			continue
		}
		if ip := desktopAddress(id, c.NetworkSettings.Networks); ip != "" {
			if a, err := netip.ParseAddr(ip); err == nil {
				byAddr[a.Unmap()] = id
			}
		}
	}
	return callerMap{byAddr: byAddr, read: true, at: time.Now(), gen: gen}
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
