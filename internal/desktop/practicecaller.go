package desktop

import (
	"context"
	"net/netip"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// The practice broker is reached by everything on the practice network - the practice desktop and its mock app - so a
// desktop_id in a callback's body proves nothing (C-25): a mock app could post outcomes, drafts or events as the
// practice desktop. The address a request comes from is Docker's to assign, so PracticeDesktopAt says which practice
// desktop has it, as DesktopAt does for the desktop network (caller.go).

// PracticeDesktopAt returns the ID (PracticeDesktopID) of the practice desktop whose container has addr on the
// practice network, and false when none does or Docker cannot be asked. Only a container the control plane labelled
// as a practice run's, named for that run and not its mock, counts. It asks Docker each time; the broker keeps the
// answer for a moment (practice.Broker.Caller).
func (m *Manager) PracticeDesktopAt(ctx context.Context, addr netip.Addr) (string, bool) {
	if m == nil || m.client == nil || !addr.IsValid() {
		return "", false
	}
	addr = addr.Unmap()
	list, err := m.client.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", PracticeLabel+"=true"))})
	if err != nil {
		return "", false
	}
	for _, c := range list {
		run := c.Labels["decurix-practice-run"]
		if c.Labels[PracticeLabel] != "true" || c.Labels["decurix-practice-mock"] == "true" || !practiceRunID.MatchString(run) {
			continue
		}
		id := PracticeDesktopID(run)
		named := false
		for _, n := range c.Names {
			if n == "/"+id || n == id {
				named = true
			}
		}
		if !named || c.NetworkSettings == nil {
			continue
		}
		ep := practiceEndpoint(c.NetworkSettings.Networks)
		if ep == nil {
			continue
		}
		if a, err := netip.ParseAddr(ep.IPAddress); err == nil && a.Unmap() == addr {
			return id, true
		}
	}
	return "", false
}
