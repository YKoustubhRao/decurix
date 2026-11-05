package desktop

import (
	"context"
	"log"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// copyPauses counts the copies that are working in a desktop a copy had to unpause (review RW1-C-04). The last copy
// to finish pauses it again - and only when a copy did the unpausing and nobody has paused or resumed it since: two
// overlapping copies no longer freeze each other, and a person's Resume during a copy is not undone.
type copyPauses struct {
	mu      sync.Mutex
	working map[string]int
	repause map[string]bool
	names   map[string]*sync.Mutex
}

var homeCopyPauses = &copyPauses{working: map[string]int{}, repause: map[string]bool{}, names: map[string]*sync.Mutex{}}

// lockName holds the start of copies into desktop container name, one at a time, and returns the unlock. The lock
// is per desktop, so copies into different desktops never wait on each other; one entry is kept per desktop name.
func (c *copyPauses) lockName(name string) func() {
	c.mu.Lock()
	l := c.names[name]
	if l == nil {
		l = &sync.Mutex{}
		c.names[name] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// begin notes a copy into container id; unpaused says this copy resumed it.
func (c *copyPauses) begin(id string, unpaused bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.working[id]++
	if unpaused {
		c.repause[id] = true
	}
}

// joining reports whether another copy already resumed container id, and counts this one in if so.
func (c *copyPauses) joining(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.working[id] == 0 || !c.repause[id] {
		return false
	}
	c.working[id]++
	return true
}

// end notes a copy finished, and reports whether the desktop is to be paused again now.
func (c *copyPauses) end(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.working[id]--
	if c.working[id] > 0 {
		return false
	}
	delete(c.working, id)
	again := c.repause[id]
	delete(c.repause, id)
	return again
}

// settled forgets a pending re-pause of container id: a person paused or resumed it themselves.
func (c *copyPauses) settled(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.repause, id)
}

// homeHelperInstanceLabel names the control plane that made a home helper, so a sweep removes only its own.
const homeHelperInstanceLabel = "decurix.homefs.instance"

// SweepHomeHelpers removes the throwaway home-copy containers this control plane left (a crash mid-copy): they hold
// a desktop's home volume, which a later delete of that desktop could then not remove (RW1-C-05). Another instance's
// are left to it. Returns how many it removed.
func (m *Manager) SweepHomeHelpers(ctx context.Context) int {
	args := filters.NewArgs()
	args.Add("label", homeHelperInstanceLabel+"="+hopInstance())
	list, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		log.Printf("desktop: listing home-copy helpers: %v", err)
		return 0
	}
	n := 0
	for _, c := range list {
		if err := m.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err == nil {
			n++
		}
	}
	if n > 0 {
		log.Printf("desktop: removed %d home-copy helper(s) an earlier run left", n)
	}
	return n
}

// removeHomeHelpersOf removes the home-copy helpers made for desktop container name, before its home volume goes.
func (m *Manager) removeHomeHelpersOf(ctx context.Context, name string) {
	args := filters.NewArgs()
	args.Add("label", homeHelperLabel+"="+name)
	list, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return
	}
	for _, c := range list {
		_ = m.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}
