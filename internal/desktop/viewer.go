package desktop

import (
	"context"
	"strings"
)

// ScreenAccess is what a desktop's screen viewer needs before it connects.
type ScreenAccess struct {
	// ID is the sanitized desktop ID the viewer's WebSocket path is built from.
	ID string
	// Name is what the person called the desktop (DESKTOP_NAME), else its ID.
	Name string
	// Password is the one its Xvnc was started with (VNC_PW). The viewer is
	// served by the control plane, which already proxies the raw RFB stream to
	// anyone who can load this page, so handing the page the password gives
	// nothing the proxy does not; it is what lets the screen open without the
	// "Credentials" prompt noVNC used to stop on.
	Password string
}

// ScreenAccess reads a desktop's name and VNC password off its container,
// running or not: a stopped desktop's viewer still opens, and offers to start
// it. ErrNotFound when there is no such desktop.
func (m *Manager) ScreenAccess(ctx context.Context, rawID string) (ScreenAccess, error) {
	id := sanitizeName(rawID)
	access := ScreenAccess{ID: id, Name: id}

	if m.native != nil {
		meta, err := m.native.readMeta(id)
		if err != nil {
			return access, err
		}
		if strings.TrimSpace(meta.Name) != "" {
			access.Name = strings.TrimSpace(meta.Name)
		}
		// The worker's screen is served by the control plane itself (VNC
		// spoken by httpapi), with a password of this run's own.
		access.Password = m.native.screenPassword
		return access, nil
	}

	inspect, err := m.client.ContainerInspect(ctx, "desktop_"+id)
	if err != nil {
		return access, notFound(id, err)
	}

	if inspect.Config != nil {
		for _, entry := range inspect.Config.Env {
			if value, found := strings.CutPrefix(entry, "VNC_PW="); found {
				access.Password = value
			} else if value, found := strings.CutPrefix(entry, "DESKTOP_NAME="); found && strings.TrimSpace(value) != "" {
				access.Name = strings.TrimSpace(value)
			}
		}
	}

	return access, nil
}
