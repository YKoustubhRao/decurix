package winhost

import (
	"context"
	"errors"
	"testing"
)

// TestASessionIsNeverJoinedForAnotherApp: two VMs, or two apps, whose ids came out the same must not share one
// private desktop and one profile (B-19); the same app under its id is joined as before.
func TestASessionIsNeverJoinedForAnotherApp(t *testing.T) {
	m := NewManager(nil)
	running := &Session{id: "vm-app", done: make(chan struct{}), appGone: make(chan struct{}),
		target: Target{`C:\app.exe`, ""}}
	m.sessions["vm-app"] = running
	if _, err := m.Start(context.Background(), "vm-app", `C:\other.exe`, ""); !errors.Is(err, errSessionTaken) {
		t.Fatalf("joined a session running another app: %v", err)
	}
	if s, err := m.Start(context.Background(), "vm-app", `C:\app.exe`, ""); err != nil || s != running {
		t.Fatalf("the same app must join its session: %v", err)
	}
	m.starting["vm-b"] = &startCall{done: make(chan struct{}), target: Target{`C:\b.exe`, ""}}
	if _, err := m.Start(context.Background(), "vm-b", `C:\c.exe`, ""); !errors.Is(err, errSessionTaken) {
		t.Fatalf("waited on a start of another app: %v", err)
	}
}
