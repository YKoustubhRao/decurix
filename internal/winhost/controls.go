package winhost

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Control is one control of an app on a private desktop, as Windows' UI Automation reports it (or, for an app that
// reports none, as a vision model read it off the picture): what it is, what it is called, where it is in the picture's
// own pixels, and its state. A password field's value is never read.
type Control struct {
	Role     string `json:"role"`
	Name     string `json:"name"`
	Box      [4]int `json:"box"` // x, y, width, height
	Value    string `json:"value,omitempty"`
	Password bool   `json:"password,omitempty"`
	Checked  *bool  `json:"checked,omitempty"`
	Expanded *bool  `json:"expanded,omitempty"`
	Selected bool   `json:"selected,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Focused  bool   `json:"focused,omitempty"`
}

// Controls is one read of an app's controls. W and H are the size of the picture the boxes are in. Fresh says the read
// began after the latest input reached the app, so it shows what that input did; Pending, that no read has finished
// yet. Source is "uia" (Windows' own tree) or "vision" (read off the picture by a model).
type Controls struct {
	W       int       `json:"w"`
	H       int       `json:"h"`
	Title   string    `json:"title,omitempty"`
	Nodes   []Control `json:"nodes"`
	Fresh   bool      `json:"fresh"`
	Pending bool      `json:"pending,omitempty"`
	Error   string    `json:"error,omitempty"`
	Source  string    `json:"source,omitempty"`
}

// captionControls are what Windows puts on every window's title bar: they say nothing about what the app shows.
var captionControls = map[string]bool{"minimize": true, "maximize": true, "restore": true, "close": true, "system": true}

// Actionable counts the controls that are the app's own and can be used - not text, not a picture, not the title bar's
// buttons. An app that draws its own screen (a canvas, a game, a toolkit that reports nothing) has none.
func (c Controls) Actionable() int {
	n := 0
	for _, node := range c.Nodes {
		switch node.Role {
		case "text", "image", "columnheader":
			continue
		}
		if captionControls[strings.ToLower(node.Name)] {
			continue
		}
		n++
	}
	return n
}

// parseControls reads the helper's "TREE <fresh 0|1> <base64 JSON>".
func parseControls(resp string) (Controls, error) {
	rest, ok := strings.CutPrefix(resp, "TREE ")
	if !ok {
		return Controls{}, errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	fresh, data, ok := strings.Cut(rest, " ")
	if !ok {
		return Controls{}, errors.New("the host sent controls the control plane could not read")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil {
		return Controls{}, errors.New("the host sent controls the control plane could not read")
	}
	var c Controls
	if err := json.Unmarshal(raw, &c); err != nil {
		return Controls{}, errors.New("the host sent controls the control plane could not read")
	}
	c.Fresh = fresh == "1" && !c.Pending
	c.Source = "uia"
	return c, nil
}

// Controls returns the app's controls from the helper's last read of them, and starts the next read. The read itself
// runs beside the app's input, never in front of it, so this answers at once; ask again until Fresh to see what an
// input did.
func (s *Session) Controls() (Controls, error) {
	resp, err := s.Do("tree")
	if err != nil {
		return Controls{}, err
	}
	return parseControls(resp)
}

// Picture is a whole JPEG of what the app shows now, at most width pixels wide, for reading it some other way than its
// controls (a vision model). The helper answers "same" for a picture it has already sent; that one is kept and given.
func (s *Session) Picture(width int) ([]byte, error) {
	resp, err := s.Do("thumb " + strconv.Itoa(width))
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp, "THUMB ") {
		return nil, errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.thumbSent) == 0 {
		return nil, errors.New("the host has no picture of the app")
	}
	return s.thumbSent, nil
}
