package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Capabilities is what the brain TheBot runs on can do, and the budgets that
// follow from it. Everything that used to assume "a 3B model on a CPU" asks
// this instead, so one TheBot serves a small local model and a hosted
// frontier model alike, each at its best:
//
//   - Vision: the model reads images. The operator then shows it the screen
//     after every on-screen action, instead of only the element list - a
//     small text model cannot read pixels, and pretending otherwise makes it
//     click the wrong thing; a model that can see catches what the
//     accessibility tree leaves out (a canvas, an unlabelled icon, an error
//     painted in red).
//   - Hosted: a hosted model reads a long prompt in seconds and has a large
//     context window, where a CPU-bound local one pays minutes per long
//     prompt and has a small window. Budgets that exist to protect the local
//     model (how much of a command's output, of the screen's element list,
//     how many turns a goal may take) are raised for it.
type Capabilities struct {
	// Brain names what TheBot reasons with: "ollama", "claude" or "azure".
	Brain         string
	Vision        bool
	Hosted        bool
	ContextTokens int
}

// CommandOutputChars is how much of one command's output the operator shows
// the model.
func (c Capabilities) CommandOutputChars() int {
	if c.Hosted {
		return 16000
	}
	return 4000
}

// ScreenTextChars is the element-list budget handed to Decurix's gui command
// (DECURIX_GUI_TEXT_CHARS): a local model is shown at most
// CommandOutputChars of any output, so the list must fit inside it.
func (c Capabilities) ScreenTextChars() int {
	if c.Hosted {
		return 12000
	}
	return 3500
}

// OperatorTurns is how many commands one goal may take. A hosted model can
// carry a long on-screen workflow (an ERP form, a multi-page upload) to the
// end; a local one that has not finished in 16 is lost, not nearly done.
func (c Capabilities) OperatorTurns() int {
	if c.Hosted {
		// 40 ran out live entering a three-line order in Odoo (a product
		// variant dialog per line): every click and field is a turn.
		return 80
	}
	return 16
}

// ScreenshotScale is the size screenshots are shown at: whole for a hosted
// model, half for a local one - a local vision model reads an image on the
// CPU, and a quarter of the pixels is a quarter of that wait.
func (c Capabilities) ScreenshotScale() int {
	if c.Hosted {
		return 1
	}
	return 2
}

// Brain holds the Capabilities, which can be learned after startup: a local
// Ollama is asked what its model can do, and it may not be up yet when soul
// starts. Nil-safe: a nil Brain is a small local text model.
type Brain struct {
	mu   sync.RWMutex
	caps Capabilities
}

// NewBrain starts from what is already known.
func NewBrain(caps Capabilities) *Brain {
	return &Brain{caps: caps}
}

// Caps returns the current Capabilities.
func (b *Brain) Caps() Capabilities {
	if b == nil {
		return Capabilities{Brain: "ollama"}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.caps
}

func (b *Brain) set(update func(*Capabilities)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	update(&b.caps)
}

// ollamaShow is the part of Ollama's /api/show answer read here. Ollama lists
// a model's capabilities ("completion", "vision", "tools", ...) and its
// architecture's context length under model_info.
type ollamaShow struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
}

// ProbeOllama asks a local Ollama what model can do: vision, and its context
// window. /api/show reads the model's manifest without loading it, so this
// costs nothing on a small host - and must stay that way: nothing may send
// the shared Ollama a chat that is not an agent's own (it evicts the cached
// prompt every desktop's next turn relies on).
func ProbeOllama(ctx context.Context, host, model string) (vision bool, contextTokens int, err error) {
	body, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+"/api/show", bytes.NewReader(body))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, 0, &httpStatusError{resp.StatusCode}
	}

	var show ollamaShow
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return false, 0, err
	}

	for _, c := range show.Capabilities {
		if c == "vision" {
			vision = true
		}
	}
	for k, v := range show.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if n, ok := v.(float64); ok {
				contextTokens = int(n)
			}
		}
	}

	return vision, contextTokens, nil
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "HTTP " + http.StatusText(e.code) }

// LearnFromOllama keeps asking a local Ollama about its model until it
// answers (it may start after soul), then records what it said. Vision is
// only ever learned here when nobody said otherwise (visionKnown false).
func (b *Brain) LearnFromOllama(ctx context.Context, host, model string, visionKnown bool, logf func(string, ...any)) {
	wait := 5 * time.Second
	for {
		vision, window, err := ProbeOllama(ctx, host, model)
		if err == nil {
			b.set(func(c *Capabilities) {
				if !visionKnown {
					c.Vision = vision
				}
				if window > 0 {
					c.ContextTokens = window
				}
			})
			caps := b.Caps()
			logf("[soul] brain: %s on Ollama - vision %v, context %d tokens", model, caps.Vision, caps.ContextTokens)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < 2*time.Minute {
			wait *= 2
		}
	}
}

// base64All encodes images for a provider's JSON.
func base64All(images [][]byte) []string {
	if len(images) == 0 {
		return nil
	}
	out := make([]string, len(images))
	for i, img := range images {
		out[i] = base64.StdEncoding.EncodeToString(img)
	}
	return out
}
