package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // pictures of host apps are JPEG
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// A host app's controls reach the VM's viewer, which lays them over the picture as real elements - so the VM's agent
// reads and uses the app by name, its approval gate knows a Pay button, and a password field is one (hostgui.html). They
// come from Windows' own UI Automation tree (winhost.Session.Controls). Some apps report nothing there - one that draws
// its own screen, a game, a toolkit without accessibility - and for those, when a hosted brain that reads pictures is
// set up, the picture is read by that model instead: the same shape, marked "vision", from a model's estimate of where
// each control is. It is the fallback, not the source: a model reads every frame anew, costs money each time and can
// misplace a box, where the tree is exact and free.

const (
	// visionEvery is the least time between two vision reads of one app: the picture of an app being used changes
	// many times a second, and every read is a paid call.
	visionEvery = 5 * time.Second
	// visionWidth is how wide the picture shown to the model is, at most.
	visionWidth = 1600
	// visionSettle is how long after an input a picture is taken to show what it did.
	visionSettle = 600 * time.Millisecond
)

// hostVision is one app's vision fallback: what the model last read, and of which picture.
type hostVision struct {
	mu        sync.Mutex
	running   bool
	last      time.Time        // when the last read began
	pic       [32]byte         // the picture it read
	picAt     time.Time        // when that picture was taken
	controls  winhost.Controls // what it read
	lastInput time.Time        // the latest input to the app, from any viewer
}

func (s *Server) visionFor(id string) *hostVision {
	s.hostVisionMu.Lock()
	defer s.hostVisionMu.Unlock()
	if s.hostVision == nil {
		s.hostVision = map[string]*hostVision{}
	}
	v := s.hostVision[id]
	if v == nil {
		v = &hostVision{}
		s.hostVision[id] = v
	}
	return v
}

// noteHostInput records that an input reached app id: a vision read from before it shows what the input changed.
func (s *Server) noteHostInput(id string) {
	v := s.visionFor(id)
	v.mu.Lock()
	v.lastInput = time.Now()
	v.mu.Unlock()
}

// visionProvider is the hosted brain that reads pictures, "" when none is set up.
func (s *Server) visionProvider() string {
	if s.LLM == nil {
		return ""
	}
	status := s.LLM.Status()
	for _, p := range []string{llm.ProviderAzure, llm.ProviderClaude} {
		if st, ok := status[p]; ok && st.Configured && st.Vision {
			return p
		}
	}
	return ""
}

// sessionControls is what the viewer of app id is given to lay over the picture: the app's own controls, or, when it
// reports none it could be used by, what a vision model read off its picture (started here when due, in the
// background, and given from the last read until then).
func (s *Server) sessionControls(sess *winhost.Session, id string) winhost.Controls {
	c, err := sess.Controls()
	if err != nil {
		return winhost.Controls{Error: viewerReason(err)}
	}
	if !c.Pending && c.Error == "" {
		// An app Windows describes takes typed text as characters; one that draws its own screen, as keys.
		sess.SetKeyedText(c.Actionable() == 0)
	}
	if c.Pending || c.Error != "" || c.Actionable() > 0 {
		return c
	}
	provider := s.visionProvider()
	if provider == "" {
		return c
	}

	v := s.visionFor(id)
	v.mu.Lock()
	due := !v.running && time.Since(v.last) >= visionEvery
	if due {
		v.running = true
		v.last = time.Now()
	}
	out := v.controls
	fresh := !v.picAt.IsZero() && v.picAt.After(v.lastInput.Add(visionSettle))
	v.mu.Unlock()
	if due {
		go s.readByVision(sess, id, provider, c)
	}

	if out.Nodes == nil {
		// Nothing read yet: say so, with what the tree had (the title bar), rather than an empty screen.
		c.Pending = true
		c.Fresh = false
		return c
	}
	out.Title = c.Title
	out.Fresh = fresh && c.Fresh
	out.Source = "vision"
	return out
}

// readByVision takes a picture of the app and, if it is not the one already read, asks the model what controls it
// shows. tree is the app's own (empty) read, for the picture's size.
func (s *Server) readByVision(sess *winhost.Session, id, provider string, tree winhost.Controls) {
	v := s.visionFor(id)
	defer func() {
		v.mu.Lock()
		v.running = false
		v.mu.Unlock()
	}()

	taken := time.Now()
	pic, err := sess.Picture(visionWidth)
	if err != nil || len(pic) == 0 {
		return
	}
	sum := sha256.Sum256(pic)
	v.mu.Lock()
	same := sum == v.pic && v.controls.Nodes != nil
	if same {
		v.picAt = taken // what was read is still what is shown
	}
	v.mu.Unlock()
	if same {
		return
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(pic))
	if err != nil || cfg.Width == 0 || cfg.Height == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	nodes, err := s.askVision(ctx, provider, pic, cfg.Width, cfg.Height)
	if err != nil {
		log.Printf("host-gui: vision read of %s: %v", id, err)
		return
	}

	// The model saw a picture of cfg's size; the viewer's boxes are in the app's own picture's pixels.
	w, h := tree.W, tree.H
	if w == 0 || h == 0 {
		w, h = cfg.Width, cfg.Height
	}
	fx, fy := float64(w)/float64(cfg.Width), float64(h)/float64(cfg.Height)
	for i := range nodes {
		b := nodes[i].Box
		nodes[i].Box = [4]int{int(float64(b[0]) * fx), int(float64(b[1]) * fy), int(float64(b[2]) * fx), int(float64(b[3]) * fy)}
	}
	v.mu.Lock()
	v.pic = sum
	v.picAt = taken
	v.controls = winhost.Controls{W: w, H: h, Nodes: nodes, Source: "vision"}
	v.mu.Unlock()
}

// visionRoles are the roles a model may give a control: the ones the viewer knows how to lay over the picture.
var visionRoles = map[string]bool{"button": true, "link": true, "textbox": true, "checkbox": true, "radio": true,
	"combobox": true, "menuitem": true, "tab": true, "option": true, "slider": true, "text": true}

const visionSystem = `You read a screenshot of one desktop application and list its controls, for a program that will click and type into them by name. Reply with JSON only: {"cursor":"...","nodes":[{"role":"...","name":"...","box":[x,y,width,height],"value":"...","password":true,"checked":true}]}.
- role is one of: button, link, textbox, checkbox, radio, combobox, menuitem, tab, option, slider, text.
- name is the control's own label as shown (a button's caption, the label beside a field, a tab's title); for an icon-only button, what it does in two or three words.
- box is in the screenshot's pixels, tight around the control itself (for a field, the box you type into, not its label).
- value is what a field or drop-down shows now; omit it when empty. Never write out what a password field holds.
- password is true only for a field whose text is hidden as dots.
- checked is given for checkboxes, radios and toggles.
- Include headings and important text lines as role "text". Leave out the window's own title-bar buttons.
- List only what is visible. If you are not sure something is a control, leave it out.
- cursor is the name of the one field that is plainly the active one - highlighted, or showing a text cursor - exactly as you named it in nodes; omit it when no field plainly is.`

// askVision has the model list the controls in a picture of width x height.
func (s *Server) askVision(ctx context.Context, provider string, pic []byte, width, height int) ([]winhost.Control, error) {
	body, _ := json.Marshal(map[string]any{
		"model":                 "default",
		"max_completion_tokens": 4000,
		"messages": []any{
			map[string]any{"role": "system", "content": visionSystem},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("The screenshot is %d x %d pixels.", width, height)},
				map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(pic)}},
			}},
		},
	})
	status, answer, _, err := s.LLM.Chat(ctx, provider, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the model answered %d", status)
	}
	var reply struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(answer, &reply); err != nil || len(reply.Choices) == 0 {
		return nil, fmt.Errorf("the model's answer could not be read")
	}
	return parseVisionControls(reply.Choices[0].Message.Content, width, height)
}

// parseVisionControls reads a model's list of controls, keeping only what is well formed: a known role, a name (or a
// field), and a box inside the picture. A password field's value is dropped whatever the model wrote.
func parseVisionControls(text string, width, height int) ([]winhost.Control, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("the model did not answer with JSON")
	}
	var got struct {
		Cursor string            `json:"cursor"`
		Nodes  []winhost.Control `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &got); err != nil {
		return nil, fmt.Errorf("the model's JSON could not be read: %w", err)
	}
	nodes := []winhost.Control{}
	for _, n := range got.Nodes {
		n.Role = strings.ToLower(strings.TrimSpace(n.Role))
		n.Name = strings.TrimSpace(n.Name)
		if !visionRoles[n.Role] || (n.Name == "" && n.Role != "textbox" && n.Role != "combobox") {
			continue
		}
		x, y, w, h := n.Box[0], n.Box[1], n.Box[2], n.Box[3]
		if w < 2 || h < 2 || x < 0 || y < 0 || x >= width || y >= height {
			continue
		}
		if x+w > width {
			n.Box[2] = width - x
		}
		if y+h > height {
			n.Box[3] = height - y
		}
		if n.Password {
			n.Value = ""
		}
		// Only Windows says what has the keyboard; a model guessing would steer typing into the wrong field.
		n.Focused = false
		nodes = append(nodes, n)
		if len(nodes) >= 300 {
			break
		}
	}
	// Which field the picture shows as active, as a line of text over that field: a program worked by keyboard (one that
	// draws its own screens) types into whatever field has the cursor, and a worker that cannot tell which one does typed
	// a ledger's group into its Name (found live, 2026-10-15). Never as "focused": that state is Windows' own word, and the
	// approval gate reads it.
	if cursor := strings.TrimSpace(got.Cursor); cursor != "" {
		for _, n := range nodes {
			if (n.Role == "textbox" || n.Role == "combobox") && strings.EqualFold(n.Name, cursor) {
				nodes = append(nodes, winhost.Control{Role: "text", Name: "the cursor is in: " + n.Name, Box: n.Box})
				break
			}
		}
	}
	return nodes, nil
}
