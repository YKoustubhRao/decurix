package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeRecorder is a native worker's agent as nativerecording.go meets it: it
// says whether a demonstration is being recorded and takes the input sent to it.
type fakeRecorder struct {
	mu        sync.Mutex
	recording bool
	asked     int
	inputs    []map[string]any
}

func (f *fakeRecorder) request(_ context.Context, _, method, path string, body io.Reader) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	answer := map[string]any{}
	switch {
	case method == http.MethodGet && path == "/demonstration":
		f.asked++
		if f.recording {
			answer["demonstration"] = map[string]any{"session": "demo-1"}
		} else {
			answer["demonstration"] = nil
		}
	case method == http.MethodPost && path == "/demonstration/input":
		var input map[string]any
		json.NewDecoder(body).Decode(&input)
		if f.recording {
			f.inputs = append(f.inputs, input)
		}
		answer["recording"] = f.recording
	}

	encoded, _ := json.Marshal(answer)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
}

func (f *fakeRecorder) set(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recording = on
}

// What a person does on a native worker's screen reaches its recorder only
// while a demonstration is being recorded, and the first click after Record
// is not lost to an answer learned before it.
func TestViewerInputReachesTheRecorderOnlyWhileRecording(t *testing.T) {
	agent := &fakeRecorder{}
	s := &Server{agentRequest: agent.request}
	record := s.recordViewerInput("ada")

	click := hostGUIRequest{Kind: "down", X: 120, Y: 80}

	// Nothing is being recorded: the agent is asked once, and not again for
	// every key of what the person types next.
	record(click)
	record(hostGUIRequest{Kind: "type", Text: "a"})
	record(hostGUIRequest{Kind: "type", Text: "b"})
	if len(agent.inputs) != 0 || agent.asked != 1 {
		t.Fatalf("with nothing recording: %d inputs sent, asked %d times; want 0 and 1", len(agent.inputs), agent.asked)
	}

	// Record is pressed (the ticket and demonstration handlers call
	// forgetRecording): the very next click is the recording's first.
	agent.set(true)
	s.forgetRecording("ada")
	record(click)
	record(hostGUIRequest{Kind: "key", VK: 13, Mods: "c"})
	if len(agent.inputs) != 2 {
		t.Fatalf("while recording: %d inputs sent, want 2", len(agent.inputs))
	}
	if got := agent.inputs[0]; got["kind"] != "down" || got["x"] != float64(120) || got["y"] != float64(80) {
		t.Errorf("the click arrived as %v", got)
	}
	if got := agent.inputs[1]; got["kind"] != "key" || got["vk"] != float64(13) || got["mods"] != "c" {
		t.Errorf("the shortcut arrived as %v", got)
	}

	// The pointer merely moving is not something a recording keeps.
	record(hostGUIRequest{Kind: "move", X: 1, Y: 1})
	if len(agent.inputs) != 2 {
		t.Errorf("a mouse move was sent to the recorder")
	}

	// The demonstration ends on the worker's side: the recorder says so in
	// its answer, and nothing more is sent.
	agent.set(false)
	record(click)
	asked := agent.asked
	record(click)
	if len(agent.inputs) != 2 || agent.asked != asked {
		t.Errorf("after it ended: %d inputs, asked again %d times; want 2 and 0", len(agent.inputs), agent.asked-asked)
	}
}
