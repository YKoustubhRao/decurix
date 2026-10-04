package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors returned by ProxyTarget so callers can map them to the
// right HTTP status/message without this package knowing about net/http.
var (
	ErrNotFound   = errors.New("desktop not found")
	ErrNotRunning = errors.New("desktop is not running")
	ErrPaused     = errors.New("desktop is paused")
	ErrNoNetwork  = errors.New("desktop is not connected to Decurix network")
	ErrNoAddress  = errors.New("desktop has no network IP")

	// ErrAgentUnreachable means the desktop container is up but nothing
	// answered on the agent's port - the agent is still starting, has
	// crashed, or never bound. Distinct from the errors above because the
	// desktop itself is fine; only its agent is not listening yet.
	ErrAgentUnreachable = errors.New("desktop agent is not responding")

	// ErrNoVNC is a native Windows worker asked for a VNC address: its
	// screen is not an X server but a private desktop, shown through
	// Manager.NativeSession. ErrNoStats is the same worker asked for a
	// container's resource figures.
	ErrNoVNC   = errors.New("a native worker's screen is not served over VNC")
	ErrNoStats = errors.New("a native worker has no container statistics")
)

// ProxyTarget resolves the internal VNC address (http://ip:port) to reverse-proxy
// a request to for the given desktop ID. It also returns the sanitized desktop
// ID, since callers need it to strip the "/desktop/<id>" URL prefix.
func (m *Manager) ProxyTarget(ctx context.Context, rawID string) (proxyURL *url.URL, sanitizedID string, err error) {
	return m.resolveProxyTarget(ctx, rawID, containerPort)
}

// AgentProxyTarget resolves the internal address of TheBot soul's HTTP port
// (agentPort) for the given desktop ID, the same way ProxyTarget resolves the
// VNC port. Only desktops created with harness=thebot have anything
// listening there.
func (m *Manager) AgentProxyTarget(ctx context.Context, rawID string) (proxyURL *url.URL, sanitizedID string, err error) {
	return m.resolveProxyTarget(ctx, rawID, agentPort)
}

// resolveProxyTarget looks up the running container for rawID and builds the
// internal http://ip:port URL to reverse-proxy to, for whichever container
// port the caller asks for (VNC vs. TheBot's own agent port).
func (m *Manager) resolveProxyTarget(ctx context.Context, rawID, port string) (proxyURL *url.URL, sanitizedID string, err error) {
	sanitizedID = sanitizeName(rawID)
	cacheKey := sanitizedID + ":" + port

	if m.native != nil {
		// A worker's ports are its own and change with every start, so
		// nothing is cached: the lookup is a map read.
		switch port {
		case vmAgentPort:
			proxyURL, err = m.native.agentURL(sanitizedID)
		case agentPort:
			proxyURL, err = m.native.soulURL(sanitizedID)
		default:
			return nil, sanitizedID, ErrNoVNC
		}
		return proxyURL, sanitizedID, err
	}

	m.proxyTargetMu.RLock()
	if entry, ok := m.proxyTargets[cacheKey]; ok && time.Now().Before(entry.expires) {
		cachedURL := entry.proxyURL
		m.proxyTargetMu.RUnlock()
		m.touchViewed(sanitizedID)
		return cachedURL, sanitizedID, nil
	}
	m.proxyTargetMu.RUnlock()

	name := "desktop_" + sanitizedID

	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil {
		return nil, sanitizedID, ErrNotFound
	}

	if inspect.State == nil || !inspect.State.Running {
		return nil, sanitizedID, ErrNotRunning
	}

	if inspect.State.Paused {
		if err := m.client.ContainerUnpause(ctx, name); err != nil {
			return nil, sanitizedID, fmt.Errorf("auto-wake paused desktop: %w", err)
		}
		// The dashboard's pill and the screen viewer read List's cached
		// status: without this they kept saying "paused" about a desktop
		// someone was already using.
		m.invalidateList()
	}

	networkInfo, ok := inspect.NetworkSettings.Networks[NetworkName]
	if !ok || networkInfo == nil {
		return nil, sanitizedID, ErrNoNetwork
	}

	ip := networkInfo.IPAddress
	if ip == "" {
		return nil, sanitizedID, ErrNoAddress
	}

	target := fmt.Sprintf(
		"http://%s:%s",
		ip,
		strings.TrimSuffix(port, "/tcp"),
	)

	proxyURL, err = url.Parse(target)
	if err != nil {
		return nil, sanitizedID, fmt.Errorf("parse proxy target: %w", err)
	}

	m.touchViewed(sanitizedID)

	m.proxyTargetMu.Lock()
	if m.proxyTargets == nil {
		m.proxyTargets = make(map[string]proxyTargetCacheEntry)
	}
	m.proxyTargets[cacheKey] = proxyTargetCacheEntry{
		proxyURL: proxyURL,
		expires:  time.Now().Add(15 * time.Second),
	}
	m.proxyTargetMu.Unlock()

	return proxyURL, sanitizedID, nil
}
