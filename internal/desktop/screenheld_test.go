package desktop

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A desktop's screen password goes to its gate with every task, even one with no withheld values of its own, and a
// gate that does not take it then is only logged: the notice goes on as before (B7 interim).
func TestTheScreenPasswordReachesTheGateWithEveryTask(t *testing.T) {
	holdScreenPassword("box", "Scr33n-pw")
	t.Cleanup(func() { releaseScreenPassword("box") })

	gate := &gateRecorder{}
	m := gatedManager(gate, nil)
	if err := m.TellGate(context.Background(), "box", practiceRequest("t1", "export the March ledger")); err != nil {
		t.Fatal(err)
	}
	if got := gate.paths(); !slices.Equal(got, []string{"/secrets", "/request"}) {
		t.Fatalf("gate got %v", got)
	}
	values, _ := gate.calls[0].body["secrets"].([]any)
	if len(values) != 1 || values[0] != "Scr33n-pw" {
		t.Fatalf("/secrets carried %v", gate.calls[0].body["secrets"])
	}

	// With the task's own values it fails closed as before; with only the screen's it does not.
	old := &gateRecorder{}
	old.answer("/secrets", http.StatusNotFound)
	m = gatedManager(old, nil)
	if err := m.TellGate(context.Background(), "box", practiceRequest("t2", "export the March ledger")); err != nil {
		t.Fatalf("a gate that did not take only the screen password stopped the request: %v", err)
	}
	if err := m.TellGate(context.Background(), "box", practiceRequest("t3", practiceSecretRequest)); err == nil {
		t.Fatal("a gate that did not take the task's own values let the request through")
	}
}

// A desktop's screen password is held from its creation (or the control plane's start) to its deletion, and a
// practice desktop's from CreatePractice to RemovePractice.
func TestTheScreenPasswordIsHeldForTheDesktopsLife(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-" + name, Name: "/" + name, State: &container.State{}},
			Config:            &container.Config{Env: []string{"VNC_PW=Held-0n-start", "HOME=/home/headless"}},
		}, nil
	}
	fake.containers = []container.Summary{{ID: "cid-desktop_kept", Names: []string{"/desktop_kept"}, Labels: map[string]string{}}}
	m := newManagerWith(fake)
	m.HoldScreenPasswords(context.Background())
	t.Cleanup(func() { releaseScreenPassword("kept") })
	if got := ScreenHeld("kept"); len(got) != 1 || got[0] != "Held-0n-start" {
		t.Fatalf("after start, kept holds %v", got)
	}
	if got := withhold.Text("VNC_PW=Held-0n-start"); got != "VNC_PW="+withhold.Marker {
		t.Fatalf("the records keep %q", got)
	}
	releaseScreenPassword("kept")
	if ScreenHeld("kept") != nil {
		t.Fatal("a deleted desktop's password is still held")
	}
}
