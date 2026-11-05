package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"thebot/internal/scheduler"
)

// observation is an observation file as the agent's gui command saves it (computer/core.py STATE_FILE).
const observation = `{"active_title": "Shop - Chromium",
 "groups": [{"id": 1, "window_title": "Checkout - Chromium"}, {"id": 2, "title": "Dialog"}],
 "elements": [
  {"ref": "e33", "role": "button", "name": "Pay ₹12,400", "group": 1},
  {"ref": "e7", "role": "textbox", "name": "Notes", "group": 2},
  {"ref": "e8", "role": "link", "name": "Help"},
  {"ref": "e9", "role": "button", "name": "Close", "window_title": "Own window", "group": 1}
 ]}`

func withObservation(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "observation.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := observationFile
	observationFile = func() string { return path }
	t.Cleanup(func() { observationFile = old })
}

// A held gui command on a ref sends what the ref is, as the agent's approval.command_target gives it (H-PY-17), so
// the gate's question names the control; the rest of the material is as before.
func TestAHeldClickOnARefSendsItsTarget(t *testing.T) {
	withObservation(t, observation)
	calls := fakeGate(t, `{"allowed": false, "request": {"id": "req-9", "fingerprint": "fp-9", "question": "May I?"}}`)
	gateAsk(&scheduler.Checkpoint{Kind: "approval", Action: "gui click e33", WorkDir: "/work"})
	material, _ := calls()[0]["material"].(map[string]any)
	want := map[string]any{"ref": "e33", "role": "button", "name": "Pay ₹12,400", "window_title": "Checkout - Chromium"}
	if !reflect.DeepEqual(material["target"], want) {
		t.Fatalf("target = %v, want %v", material["target"], want)
	}
	if material["kind"] != "command" || material["command"] != "gui click e33" || material["workdir"] != "/work" {
		t.Fatalf("the rest of the material changed: %v", material)
	}
}

func TestCommandTarget(t *testing.T) {
	withObservation(t, observation)
	cases := map[string]map[string]any{
		// the group's title ("title" when it has no window_title), else the observation's active window
		"gui type --enter e7 x": {"ref": "e7", "role": "textbox", "name": "Notes", "window_title": "Dialog"},
		"gui click e8":          {"ref": "e8", "role": "link", "name": "Help", "window_title": "Shop - Chromium"},
		"gui click e9":          {"ref": "e9", "role": "button", "name": "Close", "window_title": "Own window"},
		// no such element on the screen: said, never guessed
		"gui click e44": {"ref": "e44", "unresolved": true},
		// a command that names no ref has no target
		"rm -rf build":            nil,
		"gui type 'textbox' 'x'":  nil,
		"echo gui click e33 done": nil,
	}
	for command, want := range cases {
		if got := commandTarget(command); !reflect.DeepEqual(got, want) {
			t.Errorf("commandTarget(%q) = %v, want %v", command, got, want)
		}
	}
}

// A missing or unreadable observation leaves the ref unresolved, and the gate is still asked.
func TestAMissingObservationSendsUnresolved(t *testing.T) {
	for _, content := range []string{"", "not json"} {
		withObservation(t, content)
		calls := fakeGate(t, `{"allowed": false, "request": {"id": "req-9", "fingerprint": "fp-9", "question": "May I?"}}`)
		gateAsk(&scheduler.Checkpoint{Kind: "approval", Action: "gui click e33", WorkDir: "/work"})
		material, _ := calls()[0]["material"].(map[string]any)
		if want := map[string]any{"ref": "e33", "unresolved": true}; !reflect.DeepEqual(material["target"], want) {
			t.Fatalf("%q: target = %v, want %v", content, material["target"], want)
		}
	}
}
