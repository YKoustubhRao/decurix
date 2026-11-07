package desktop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Room for a desktop that is about to start (lightweight L2). A paused desktop frees its CPU but keeps all its
// memory, so on a small host only a few fit. Before a desktop is started, the RoomMaker the control plane set
// (idlepause.Checker.MakeRoom) is asked to free memory for it - it stops the longest-paused desktops nobody is using -
// or to refuse the start with a NoRoomError. A wake (unpause) never asks: a paused desktop's memory is still held by
// its frozen processes, so waking it needs no new room, and stopping another desktop for it would free nothing it uses.

// TypicalFootprintMB is what a desktop is taken to need when it starts: an idle desktop measured about 400 MB, one with
// Chromium open about 630 MB (bench/resources), so 700 MB covers a desktop that boots and opens its browser. A
// desktop's memory limit is larger (it is a ceiling, not what it uses); a desktop with a smaller limit needs only that.
const TypicalFootprintMB = 700

// RoomMaker frees memory for desktop forID, about to start and needing needMB, or says why it cannot (a NoRoomError).
type RoomMaker func(ctx context.Context, forID string, needMB int64) error

// ErrNoRoom is a start refused because the host has not enough memory for one more desktop and none can be stopped.
var ErrNoRoom = errors.New("not enough memory")

// NoRoomError is ErrNoRoom with how many desktops are using the memory. Its text is the control plane's own, for the
// person to read: "not enough memory: 3 desktops are busy".
type NoRoomError struct {
	Busy int
}

// Error says that there is not enough memory and how many desktops are busy.
func (e *NoRoomError) Error() string {
	busy := fmt.Sprintf("%d desktops are", e.Busy)
	if e.Busy == 1 {
		busy = "1 desktop is"
	}
	return "Not enough memory to start this desktop: " + busy + " busy. Try again when one finishes, or stop one from the dashboard."
}

// Is makes a NoRoomError match ErrNoRoom.
func (e *NoRoomError) Is(target error) bool { return target == ErrNoRoom }

// Why a desktop was stopped to save memory (Desktop.RestReason).
const (
	RestIdle = "idle"   // paused past DECURIX_IDLE_STOP_AFTER_MINUTES
	RestRoom = "memory" // stopped to make room for another desktop
)

// Rest says why the control plane stopped a desktop to save memory: Reason is RestIdle or RestRoom, Note one line for
// the person ("Stopped after 30 minutes paused, to save memory"). Kept in memory until the desktop starts again; a
// restarted control plane knows none.
type Rest struct {
	Reason string
	Note   string
	At     time.Time
}

// roomState is the Manager's bookkeeping for making room: the RoomMaker, why each stopped desktop rests, and when
// each desktop was paused or woken, by sanitized ID.
type roomState struct {
	maker atomic.Pointer[RoomMaker]

	mu     sync.Mutex
	rests  map[string]Rest
	paused map[string]pauseMark
	woken  map[string]time.Time
}

// pauseMark is when a desktop was paused, and when it had last been looked at then: anything that looks at a paused
// desktop afterwards is its frozen thumbnail, since opening its screen wakes it.
type pauseMark struct {
	at, viewed time.Time
}

// SetRoomMaker sets what is asked to free memory before a desktop starts; nil starts desktops without asking.
func (m *Manager) SetRoomMaker(f RoomMaker) {
	if f == nil {
		m.room.maker.Store(nil)
		return
	}
	m.room.maker.Store(&f)
}

// makeRoomFor asks the RoomMaker for room for container ref (a "desktop_<id>" name or a container ID) before it is
// started. A desktop already running needs none; one Docker cannot read is left to the start itself to fail.
func (m *Manager) makeRoomFor(ctx context.Context, ref string) error {
	fp := m.room.maker.Load()
	if fp == nil {
		return nil
	}
	inspect, err := m.client.ContainerInspect(ctx, ref)
	if err != nil || inspect.State == nil || inspect.State.Running {
		return nil
	}
	need := int64(TypicalFootprintMB)
	if inspect.HostConfig != nil && inspect.HostConfig.Memory > 0 && inspect.HostConfig.Memory/(1<<20) < need {
		need = inspect.HostConfig.Memory / (1 << 20)
	}
	name := strings.TrimPrefix(inspect.Name, "/")
	if name == "" {
		name = ref
	}
	id := strings.TrimPrefix(name, "desktop_")
	return (*fp)(ctx, id, need)
}

// SetRest records why desktop id is about to be stopped to save memory, so its card can say so (Desktop.RestReason).
func (m *Manager) SetRest(id string, r Rest) {
	key := sanitizeName(id)
	m.room.mu.Lock()
	if m.room.rests == nil {
		m.room.rests = map[string]Rest{}
	}
	if r.At.IsZero() {
		r.At = time.Now()
	}
	m.room.rests[key] = r
	m.room.mu.Unlock()
	m.invalidateList()
}

// ClearRest forgets why desktop id rested (its stop failed, or it runs again).
func (m *Manager) ClearRest(id string) {
	m.room.mu.Lock()
	delete(m.room.rests, sanitizeName(id))
	m.room.mu.Unlock()
}

// RestOf is why desktop id was stopped to save memory, if it was.
func (m *Manager) RestOf(id string) (Rest, bool) {
	m.room.mu.Lock()
	defer m.room.mu.Unlock()
	r, ok := m.room.rests[sanitizeName(id)]
	return r, ok
}

// markPaused records that desktop id was paused now, and when it was last looked at.
func (m *Manager) markPaused(id string) {
	viewed, _ := m.LastViewed(id)
	m.room.mu.Lock()
	if m.room.paused == nil {
		m.room.paused = map[string]pauseMark{}
	}
	m.room.paused[sanitizeName(id)] = pauseMark{at: time.Now(), viewed: viewed}
	m.room.mu.Unlock()
}

// markRunning records that container ref ("desktop_<id>") runs again: started or woken now. It rests no longer and is
// paused no longer. A ref that is a container ID (a copy's brief wake) is not recorded.
func (m *Manager) markRunning(ref string) {
	if !strings.HasPrefix(ref, "desktop_") {
		return
	}
	key := sanitizeName(strings.TrimPrefix(ref, "desktop_"))
	m.room.mu.Lock()
	delete(m.room.rests, key)
	delete(m.room.paused, key)
	if m.room.woken == nil {
		m.room.woken = map[string]time.Time{}
	}
	m.room.woken[key] = time.Now()
	m.room.mu.Unlock()
}

// markStopped forgets desktop id's pause: a stopped desktop is not paused.
func (m *Manager) markStopped(id string) {
	m.room.mu.Lock()
	delete(m.room.paused, sanitizeName(id))
	m.room.mu.Unlock()
}

// PausedSince is when desktop id, which the caller saw paused, was paused, and when it had last been looked at
// before that (zero when not known). A desktop paused before this control plane started counts from the first time
// it is asked about.
func (m *Manager) PausedSince(id string) (at, viewedBefore time.Time) {
	key := sanitizeName(id)
	m.room.mu.Lock()
	defer m.room.mu.Unlock()
	if p, ok := m.room.paused[key]; ok {
		return p.at, p.viewed
	}
	if m.room.paused == nil {
		m.room.paused = map[string]pauseMark{}
	}
	p := pauseMark{at: time.Now()}
	m.room.paused[key] = p
	return p.at, p.viewed
}

// LastWoken is when desktop id was last started or woken through this control plane (a person's Start or Resume, a
// request's auto-start, the screen's or the gate's auto-wake).
func (m *Manager) LastWoken(id string) (time.Time, bool) {
	m.room.mu.Lock()
	defer m.room.mu.Unlock()
	t, ok := m.room.woken[sanitizeName(id)]
	return t, ok
}

// IsPaused asks Docker whether desktop id is paused right now.
func (m *Manager) IsPaused(ctx context.Context, id string) (bool, error) {
	inspect, err := m.client.ContainerInspect(ctx, "desktop_"+sanitizeName(id))
	if err != nil {
		return false, notFound(id, err)
	}
	return inspect.State != nil && inspect.State.Paused, nil
}

// HostMemTotalMB is the Docker host's total memory in MB, as its daemon reports it.
func (m *Manager) HostMemTotalMB(ctx context.Context) (int64, error) {
	info, err := m.client.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.MemTotal / (1 << 20), nil
}

// withRest puts why a stopped desktop rests on d, when the control plane stopped it to save memory.
func (m *Manager) withRest(d *Desktop) {
	if d.Status != "stopped" {
		return
	}
	if r, ok := m.RestOf(d.ID); ok {
		d.RestReason, d.RestNote, d.RestedAt = r.Reason, r.Note, r.At.UTC().Format(time.RFC3339)
	}
}
