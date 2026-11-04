package withhold

import (
	"sort"
	"strings"
	"sync"
)

// Some values are secret for as long as their owner lives, whoever wrote them where: a desktop's screen (VNC)
// password is one. Its worker runs as the uid its screen runs as, so a worker that reads it (from /proc/<pid>/environ
// of the screen, or the screen's password file) could carry it into a model call, a report or a chat (review
// RES-CP-1, B7). Such a value is held here by its owner and withheld wherever the control plane withholds (Text) or
// records what a desktop wrote (Known), whatever word is beside it. Only a value of minAlways characters or more is
// held: a short one ("admin", the shared default of old desktops, which is no secret) would cut ordinary words.

// minAlways is the shortest value HoldAlways keeps.
const minAlways = 8

var always struct {
	mu sync.RWMutex
	by map[string]string // owner -> value
}

// HoldAlways keeps value as owner's, in memory only, until ReleaseAlways(owner); a value shorter than minAlways
// characters is not kept (and owner's earlier one is let go).
func HoldAlways(owner, value string) {
	value = strings.TrimSpace(value)
	always.mu.Lock()
	defer always.mu.Unlock()
	if len(value) < minAlways {
		delete(always.by, owner)
		return
	}
	if always.by == nil {
		always.by = map[string]string{}
	}
	always.by[owner] = value
}

// ReleaseAlways lets owner's held value go.
func ReleaseAlways(owner string) {
	always.mu.Lock()
	defer always.mu.Unlock()
	delete(always.by, owner)
}

// AlwaysFor is owner's held value, or nil.
func AlwaysFor(owner string) []string {
	always.mu.RLock()
	defer always.mu.RUnlock()
	if v, ok := always.by[owner]; ok {
		return []string{v}
	}
	return nil
}

// Known is text with every held value (of every owner) withheld as Marker, longest first.
func Known(text string) string {
	if text == "" {
		return text
	}
	always.mu.RLock()
	if len(always.by) == 0 {
		always.mu.RUnlock()
		return text
	}
	values := make([]string, 0, len(always.by))
	for _, v := range always.by {
		values = append(values, v)
	}
	always.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, v := range values {
		if strings.Contains(text, v) {
			text = strings.ReplaceAll(text, v, Marker)
		}
	}
	return text
}
