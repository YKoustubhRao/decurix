package desktop

import (
	"context"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A desktop's screen (VNC) password is readable by its worker: Xvnc, the XFCE session and noVNC run as the worker's
// uid with VNC_PW in their environment, and the password file is the worker's too (TigerVNC's obfuscation is a fixed
// key). Until the screen runs as a uid of its own (an owner decision, docs-notes), the control plane holds the
// password as a secret for the desktop's whole life (withhold.HoldAlways): withheld from every record the control
// plane keeps, from the model calls it records, and given to the desktop's gate with every task's values, so a worker
// that reads it cannot carry it into a record or an approval question (review RES-CP-1, B7 interim).

// screenOwner is the withhold owner of desktop id's screen password; practice and real desktops' names never meet
// (ReservedName).
func screenOwner(id string) string { return "screen:" + sanitizeName(id) }

// ScreenHeld is desktop id's screen password as held, or nil: for the stores that withhold a desktop's own values.
func ScreenHeld(id string) []string { return withhold.AlwaysFor(screenOwner(id)) }

// holdScreenPassword holds password as desktop id's screen password.
func holdScreenPassword(id, password string) { withhold.HoldAlways(screenOwner(id), password) }

// releaseScreenPassword lets desktop id's screen password go: it is gone with its container.
func releaseScreenPassword(id string) { withhold.ReleaseAlways(screenOwner(id)) }

// HoldScreenPasswords holds every existing desktop's screen password, read off its container (a restarted control
// plane holds nothing in memory). A desktop made before screen passwords were its own has the shared "admin", which
// is no secret and too short to hold. Best effort: a desktop that cannot be read is skipped.
func (m *Manager) HoldScreenPasswords(ctx context.Context) {
	list, err := m.List(ctx)
	if err != nil {
		return
	}
	for _, d := range list {
		inspect, err := m.client.ContainerInspect(ctx, "desktop_"+sanitizeName(d.ID))
		if err != nil || inspect.Config == nil {
			continue
		}
		for _, kv := range inspect.Config.Env {
			if pw, ok := strings.CutPrefix(kv, "VNC_PW="); ok {
				holdScreenPassword(d.ID, pw)
			}
		}
	}
}
