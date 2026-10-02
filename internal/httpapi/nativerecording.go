package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Recording a person's demonstration on a native Windows worker.
//
// A desktop container's recorder listens to its X server, which sees every
// click and key whoever made it. A native worker's screen is a private desktop
// no keyboard is attached to: what a person does there arrives through this
// control plane (the viewer's WebSocket, serveSessionSocket) and is posted to
// the desktop as window messages, which nothing on the worker can listen in
// on. So this is where the person's input is known, and while a demonstration
// is being recorded each click and key is handed to the worker's recorder
// (POST /demonstration/input, demonstration.feed) before it is applied -
// before, so the recorder's picture of the screen is of the screen the person
// acted on, not of what their action did to it.

// recordingState is what was last learned about one worker.
type recordingState struct {
	on      bool
	checked time.Time
}

// How long an answer about a worker is trusted. A demonstration started from
// this control plane is known at once (forgetRecording); this bounds one that
// ended on its own, and the cost of asking: at most one question a second,
// and only while a person is actually working the screen.
const recordingStateFor = time.Second

// What the recorder is given at most this long to take in one input (it
// pictures the screen first). Past it the input is applied anyway: a person's
// click must never hang on the recording of it.
const recordInputTimeout = 4 * time.Second

// recordedKinds are the viewer's messages a recording keeps. A mouse move is
// not one: a recording is of what was done, and a drag is its press and release.
var recordedKinds = map[string]bool{"down": true, "up": true, "wheel": true, "type": true, "paste": true, "key": true, "copy": true}

// recordViewerInput returns what serveSessionSocket calls with each input from a native worker's viewer, before it
// applies it.
func (s *Server) recordViewerInput(id string) func(hostGUIRequest) {
	return func(req hostGUIRequest) {
		if !recordedKinds[req.Kind] || !s.recordingOn(id) {
			return
		}

		body, err := json.Marshal(map[string]any{
			"kind": req.Kind, "x": req.X, "y": req.Y, "right": req.Right, "mods": req.Mods,
			"delta": req.Delta, "text": req.Text, "vk": req.VK,
		})
		if err != nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), recordInputTimeout)
		defer cancel()

		var answer struct {
			Recording bool `json:"recording"`
		}
		if !s.askAgent(ctx, id, http.MethodPost, "/demonstration/input", body, &answer) || !answer.Recording {
			// It ended (or the agent is gone): stop sending until it is known to be on again.
			s.setRecording(id, false)
		}
	}
}

// recordWorkerInput is recordViewerInput for a worker's whole desktop, where a press can land on the wallpaper: on one
// of its icons (which opens something - recorded as the "open" it causes, not as a click on a picture) or on nothing.
// Neither is a step of a demonstration, and a replay would click a bare spot of the screen for it.
func (s *Server) recordWorkerInput(id string, over interface{ OverWindow(x, y int) bool }) func(hostGUIRequest) {
	record := s.recordViewerInput(id)
	onWallpaper := false // a press that began on the wallpaper ends there too

	return func(req hostGUIRequest) {
		switch req.Kind {
		case "down":
			onWallpaper = s.recordingOn(id) && !over.OverWindow(req.X, req.Y)
			if onWallpaper {
				return
			}
		case "up":
			if onWallpaper {
				onWallpaper = false
				return
			}
		}

		record(req)
	}
}

// recordingOn reports whether a demonstration is being recorded on the worker, asking its agent when the last
// answer is old.
func (s *Server) recordingOn(id string) bool {
	s.recordingMu.Lock()
	state, known := s.recordingNow[id]
	s.recordingMu.Unlock()

	if known && time.Since(state.checked) < recordingStateFor {
		return state.on
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var answer struct {
		Demonstration *struct{} `json:"demonstration"`
	}
	on := s.askAgent(ctx, id, http.MethodGet, "/demonstration", nil, &answer) && answer.Demonstration != nil
	s.setRecording(id, on)

	return on
}

func (s *Server) setRecording(id string, on bool) {
	s.recordingMu.Lock()
	defer s.recordingMu.Unlock()

	if s.recordingNow == nil {
		s.recordingNow = map[string]recordingState{}
	}
	s.recordingNow[id] = recordingState{on: on, checked: time.Now()}
}

// forgetRecording makes the next input ask the worker afresh: called after anything that starts or ends a
// demonstration there, so the first click after Record is recorded.
func (s *Server) forgetRecording(id string) {
	s.recordingMu.Lock()
	defer s.recordingMu.Unlock()

	delete(s.recordingNow, id)
}

// askAgent makes one JSON call to a worker's agent and decodes its answer into out, reporting whether that worked.
func (s *Server) askAgent(ctx context.Context, id, method, path string, body []byte, out any) bool {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request := s.agentRequest
	if request == nil {
		request = s.Desktops.AgentRequest
	}

	resp, err := request(ctx, id, method, path, reader)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out) == nil
}
