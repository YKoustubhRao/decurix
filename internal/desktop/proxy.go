package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Sentinel errors returned by ProxyTarget so callers can map them to the
// right HTTP status/message without this package knowing about net/http.
var (
	ErrNotFound   = errors.New("desktop not found")
	ErrNotRunning = errors.New("desktop is not running")
	ErrNoNetwork  = errors.New("desktop is not connected to Flowera network")
	ErrNoAddress  = errors.New("desktop has no network IP")

	// ErrAgentUnreachable means the desktop container is up but nothing
	// answered on the agent's port - the agent is still starting, has
	// crashed, or never bound. Distinct from the errors above because the
	// desktop itself is fine; only its agent is not listening yet.
	ErrAgentUnreachable = errors.New("desktop agent is not responding")
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
	name := "desktop_" + sanitizedID

	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil {
		return nil, sanitizedID, ErrNotFound
	}

	if inspect.State == nil || !inspect.State.Running {
		return nil, sanitizedID, ErrNotRunning
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

	return proxyURL, sanitizedID, nil
}
