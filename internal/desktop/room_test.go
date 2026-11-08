package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// boxIn is a daemon whose desktop "box" is in state (stopped, or paused) with a memory limit of limitMB.
func boxIn(paused bool, limitMB int64) *fakeAPI {
	fake := &fakeAPI{}
	fake.inspect = func(string) (container.InspectResponse, error) {
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", Name: "/desktop_box",
				State:      &container.State{Running: paused, Paused: paused},
				HostConfig: &container.HostConfig{Resources: container.Resources{Memory: limitMB << 20}}},
		}, nil
	}
	return fake
}

// roomAsk records what a start asked the RoomMaker.
type roomAsk struct {
	id   string
	need int64
}

// TestRoomIsMadeBeforeAStart: a stopped desktop's start asks for room first, for its own ID, needing the typical
// footprint (its limit is larger), and then starts.
func TestRoomIsMadeBeforeAStart(t *testing.T) {
	fake := boxIn(false, 2048)
	m := newManagerWith(fake)
	var asks []roomAsk
	m.SetRoomMaker(func(_ context.Context, id string, need int64) error {
		if len(fake.called("ContainerStart")) != 0 {
			t.Error("room was asked for after the start")
		}
		asks = append(asks, roomAsk{id, need})
		return nil
	})
	if err := m.Start(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if len(asks) != 1 || asks[0] != (roomAsk{"box", TypicalFootprintMB}) {
		t.Fatalf("asked %v, want [{box %d}]", asks, TypicalFootprintMB)
	}
	if len(fake.called("ContainerStart")) != 1 {
		t.Fatal("the desktop was not started")
	}
	if _, ok := m.LastWoken("box"); !ok {
		t.Fatal("the start was not recorded")
	}
}

// TestRoomNeededIsTheLimitWhenSmaller: a desktop limited below the footprint needs only its limit.
func TestRoomNeededIsTheLimitWhenSmaller(t *testing.T) {
	m := newManagerWith(boxIn(false, 512))
	var need int64
	m.SetRoomMaker(func(_ context.Context, _ string, n int64) error { need = n; return nil })
	if err := m.Start(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if need != 512 {
		t.Fatalf("need %d, want 512", need)
	}
}

// TestNoRoomRefusesTheStart: a start the RoomMaker refuses is not made, and says so as ErrNoRoom.
func TestNoRoomRefusesTheStart(t *testing.T) {
	fake := boxIn(false, 2048)
	m := newManagerWith(fake)
	m.SetRoomMaker(func(context.Context, string, int64) error { return &NoRoomError{Busy: 3} })
	err := m.Start(context.Background(), "box")
	if !errors.Is(err, ErrNoRoom) || err.Error() != "Not enough memory to start this desktop: 3 desktops are busy. Try again when one finishes, or stop one from the dashboard." {
		t.Fatalf("got %v, want the no-room refusal", err)
	}
	if len(fake.called("ContainerStart")) != 0 {
		t.Fatal("a refused start started the desktop")
	}
}

// TestAWakeMakesNoRoom: a paused desktop's memory is still held, so a wake (Start or Unpause of a paused desktop)
// does not ask for room and is never refused for it.
func TestAWakeMakesNoRoom(t *testing.T) {
	fake := boxIn(true, 2048)
	m := newManagerWith(fake)
	m.SetRoomMaker(func(context.Context, string, int64) error {
		t.Error("a wake asked for room")
		return &NoRoomError{}
	})
	if err := m.Start(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if err := m.Unpause(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.called("ContainerUnpause")); n != 2 {
		t.Fatalf("%d unpauses, want 2", n)
	}
}

// TestRestIsShownUntilTheDesktopRunsAgain: why a desktop rests is on its listing while it is stopped, and gone once it
// starts again.
func TestRestIsShownUntilTheDesktopRunsAgain(t *testing.T) {
	fake := boxIn(false, 2048)
	fake.containers = []container.Summary{{ID: "cid-box", Names: []string{"/desktop_box"}, State: "exited",
		Status: "Exited (0) 1 minute ago", Labels: map[string]string{"desktop-manager": "true", "desktop-model": "m"}}}
	m := newManagerWith(fake)
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	m.SetRest("box", Rest{Reason: RestRoom, Note: "Stopped to make room for nova, to save memory", At: at})
	list, err := m.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if d := list[0]; d.RestReason != RestRoom || d.RestNote == "" || d.RestedAt != "2026-11-07T10:00:00Z" {
		t.Fatalf("listing %+v does not say why it rests", d)
	}
	if err := m.Start(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.RestOf("box"); ok {
		t.Fatal("a desktop that runs again still rests")
	}
}

// TestTryLockDesktopPassesOverAHeldLock: TryLockDesktop never waits for a lock someone holds.
func TestTryLockDesktopPassesOverAHeldLock(t *testing.T) {
	m := newManagerWith(&fakeAPI{})
	unlock := m.LockDesktop("box")
	if _, ok := m.TryLockDesktop("box"); ok {
		t.Fatal("took a lock someone holds")
	}
	unlock()
	u, ok := m.TryLockDesktop("box")
	if !ok {
		t.Fatal("could not take a free lock")
	}
	u()
}

// With no busy desktop the refusal names other programs, not Decurix, as what holds the memory (found live 2026-11-07).
func TestNoRoomWithNoBusyDesktopNamesOtherPrograms(t *testing.T) {
	got := (&NoRoomError{FreeMB: 676, NeedMB: 700}).Error()
	if !strings.Contains(got, "about 676 MB is free") || !strings.Contains(got, "Close other programs") || strings.Contains(got, "0 desktops") {
		t.Fatalf("got %q", got)
	}
}
