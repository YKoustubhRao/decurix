// Package grounder asks a GUI-grounding model where something is on a screen.
//
// A brain that plans in words (a hosted GPT, Claude, a local chat model) is
// poor at reading pixel positions off a screenshot: it clicked (68,171) for a
// box at (100,248), and the taskbar for a row below the page's edge. A
// grounding model is trained for exactly that one job - a screenshot and "the
// Save button in the dialog" in, a point out - so the brain says WHAT to click
// and this says WHERE (the planner/grounder split of SeeAct-V, UGround,
// GUI-Actor, GTA1).
//
// Which model is a setting, not code: DECURIX_GROUNDER_MODEL names it, and
// DECURIX_GROUNDER_API says how to reach it - "ollama" (the default, the same
// Ollama the desktops use; Qwen3-VL runs there on a CPU) or "openai" (any
// OpenAI-compatible server: vLLM or a hosted endpoint serving GTA1, Holo1.5,
// UI-TARS or UGround on a GPU). Models answer in different coordinates, which
// is the one thing that differs per family (Coords). Off when no model is set.
//
// Depends on nothing internal.
package grounder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// How a model writes a position.
const (
	// CoordsNorm1000: 0..1000 of the image's width and height (Qwen3-VL, UGround-V1).
	CoordsNorm1000 = "norm1000"
	// CoordsQwen25: pixels of the image after Qwen2.5-VL's smart_resize to multiples of 28
	// (Qwen2.5-VL and what is trained on it: GTA1, Holo1.5, UI-TARS-1.5).
	CoordsQwen25 = "qwen25"
	// CoordsPixels: pixels of the image as it was sent.
	CoordsPixels = "pixels"
)

// ErrNotConfigured is returned when no grounding model is set up.
var ErrNotConfigured = errors.New("no grounding model is set up (DECURIX_GROUNDER_MODEL)")

// ErrNoPoint is returned when the model's answer holds no position.
var ErrNoPoint = errors.New("the grounding model did not give a position")

// MaxImageBytes bounds one request's screenshot.
const MaxImageBytes = 12 << 20

// Client asks one grounding model.
type Client struct {
	API    string // "ollama" or "openai"
	URL    string // Ollama's base URL, or the OpenAI-compatible base URL (…/v1)
	Model  string
	Key    string // openai only; never sent to a desktop
	Coords string
	// MaxSide shrinks a larger screenshot so its longer side is this many
	// pixels before it is sent (0: as it is). A vision encoder's work grows
	// with the pixels: on a CPU, the lever between minutes and seconds.
	MaxSide int
	Timeout time.Duration
	// KeepAlive is how long Ollama keeps the grounder loaded: short, so a
	// memory-starved host gets the memory back for the chat model.
	KeepAlive string
	HTTP      *http.Client
}

// FromEnv builds the client the control plane's .env describes, or nil when
// DECURIX_GROUNDER_MODEL is unset. ollamaURL is the Ollama the desktops use,
// the default for the "ollama" API.
func FromEnv(ollamaURL string) *Client {
	model := strings.TrimSpace(os.Getenv("DECURIX_GROUNDER_MODEL"))
	if model == "" {
		return nil
	}

	c := &Client{
		API:       strings.ToLower(strings.TrimSpace(os.Getenv("DECURIX_GROUNDER_API"))),
		URL:       strings.TrimRight(strings.TrimSpace(os.Getenv("DECURIX_GROUNDER_URL")), "/"),
		Model:     model,
		Key:       os.Getenv("DECURIX_GROUNDER_KEY"),
		Coords:    strings.ToLower(strings.TrimSpace(os.Getenv("DECURIX_GROUNDER_COORDS"))),
		KeepAlive: getenv("DECURIX_GROUNDER_KEEP_ALIVE", "5m"),
		Timeout:   180 * time.Second,
	}
	if c.API == "" {
		c.API = "ollama"
	}
	if c.URL == "" && c.API == "ollama" {
		c.URL = strings.TrimRight(ollamaURL, "/")
	}
	if c.Coords == "" {
		c.Coords = CoordsFor(model)
	}
	if s, err := strconv.Atoi(os.Getenv("DECURIX_GROUNDER_MAX_SIDE")); err == nil && s > 0 {
		c.MaxSide = s
	}
	if s, err := strconv.Atoi(os.Getenv("DECURIX_GROUNDER_TIMEOUT_SECONDS")); err == nil && s > 0 {
		c.Timeout = time.Duration(s) * time.Second
	}
	return c
}

func getenv(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// CoordsFor guesses a model family's coordinates from its name; Qwen3-VL's
// 0..1000 when nothing says otherwise. DECURIX_GROUNDER_COORDS overrides it.
func CoordsFor(model string) string {
	m := strings.ToLower(model)
	for _, family := range []string{"qwen2.5-vl", "qwen2.5vl", "qwen2_5", "gta1", "holo1", "ui-tars", "uitars", "ui-venus", "opencua"} {
		if strings.Contains(m, family) {
			return CoordsQwen25
		}
	}
	return CoordsNorm1000
}

// Status is what a desktop and the dashboard may know: never the key.
func (c *Client) Status() map[string]any {
	if c == nil {
		return map[string]any{"configured": false}
	}
	return map[string]any{"configured": true, "model": c.Model, "api": c.API, "coords": c.Coords}
}

// Point is where the model put the thing it was asked for, in the pixels of
// the image it was given.
type Point struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Answer string `json:"answer"` // the model's own words, for the logs and the tests
}

// Locate asks the model where `description` is in a PNG of width x height.
func (c *Client) Locate(ctx context.Context, png []byte, width, height int, description string) (Point, error) {
	if c == nil || c.Model == "" {
		return Point{}, ErrNotConfigured
	}
	if width <= 0 || height <= 0 {
		return Point{}, errors.New("the image's size is needed")
	}
	description = strings.Join(strings.Fields(description), " ")
	if description == "" {
		return Point{}, errors.New("say what to look for")
	}

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	sent, sw, sh, err := Shrink(png, width, height, c.MaxSide)
	if err != nil {
		return Point{}, err
	}

	var answer string
	if c.API == "openai" {
		answer, err = c.askOpenAI(ctx, sent, Prompt(c.Coords, description))
	} else {
		answer, err = c.askOllama(ctx, sent, Prompt(c.Coords, description))
	}
	if err != nil {
		return Point{}, err
	}

	x, y, ok := Parse(answer)
	if !ok {
		return Point{Answer: answer}, ErrNoPoint
	}
	px, py := ToPixels(c.Coords, x, y, sw, sh)
	// Back from the picture that was sent to the screen's own pixels.
	px = clamp(int(math.Round(float64(px)*float64(width)/float64(sw))), width)
	py = clamp(int(math.Round(float64(py)*float64(height)/float64(sh))), height)
	return Point{X: px, Y: py, Answer: answer}, nil
}

// Prompt is the question for one family. Qwen3-VL answers grounding as JSON
// with point_2d; the others take UGround's wording, which asks for "(x, y)".
func Prompt(coords, description string) string {
	if coords == CoordsNorm1000 {
		return "This is a screenshot of a computer screen. Point to the user interface element described below: " +
			"the centre of the control a person would click for it.\n" +
			"Description: " + description + "\n" +
			`Answer with JSON only: {"point_2d": [x, y]}`
	}
	return "Your task is to help the user identify the precise coordinates (x, y) of a specific area/element/object " +
		"on the screen based on a description.\n\n" +
		"- Your response should aim to point to the center or a representative point within the described " +
		"area/element/object as accurately as possible.\n" +
		"- If the description is unclear or ambiguous, infer the most relevant area or element based on its " +
		"likely context or purpose.\n" +
		"- Your answer should be a single string (x, y) corresponding to the point of the interest.\n\n" +
		"Description: " + description + "\n\nAnswer:"
}

var (
	thinkRE  = regexp.MustCompile(`(?s)<think>.*?</think>`)
	numberRE = regexp.MustCompile(`-?\d+(?:\.\d+)?`)
)

// Parse reads a position out of a model's answer, whatever shape it took:
// {"point_2d": [x, y]}, {"bbox_2d": [x1, y1, x2, y2]} (its centre), "(x, y)",
// "<click>x, y</click>", "click(start_box='(x,y)')". The first two or four
// numbers after a point/box key, else the first pair in the answer.
func Parse(answer string) (float64, float64, bool) {
	text := thinkRE.ReplaceAllString(answer, "")
	lower := strings.ToLower(text)

	for _, key := range []string{"point_2d", "point", "bbox_2d", "bbox", "box", "click", "("} {
		at := strings.Index(lower, key)
		if at < 0 {
			continue
		}
		// After the key itself: "point_2d" holds a 2 of its own.
		if x, y, ok := fromNumbers(numberRE.FindAllString(text[at+len(key):], 4), strings.Contains(key, "box")); ok {
			return x, y, true
		}
	}
	return fromNumbers(numberRE.FindAllString(text, 2), false)
}

func fromNumbers(found []string, box bool) (float64, float64, bool) {
	n := make([]float64, 0, len(found))
	for _, s := range found {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || v < 0 {
			return 0, 0, false
		}
		n = append(n, v)
	}
	if box && len(n) == 4 {
		return (n[0] + n[2]) / 2, (n[1] + n[3]) / 2, true
	}
	if len(n) >= 2 {
		return n[0], n[1], true
	}
	return 0, 0, false
}

// ToPixels turns a model's position into pixels of the width x height image,
// kept inside it.
func ToPixels(coords string, x, y float64, width, height int) (int, int) {
	switch coords {
	case CoordsNorm1000:
		x, y = x/1000*float64(width), y/1000*float64(height)
	case CoordsQwen25:
		rw, rh := SmartResize(width, height)
		x, y = x*float64(width)/float64(rw), y*float64(height)/float64(rh)
	}
	return clamp(int(math.Round(x)), width), clamp(int(math.Round(y)), height)
}

func clamp(v, size int) int {
	if v < 0 {
		return 0
	}
	if v >= size {
		return size - 1
	}
	return v
}

// SmartResize is Qwen2.5-VL's image resize: each side rounded to a multiple
// of 28, the area kept between 56*56 and 28*28*16384 pixels.
func SmartResize(width, height int) (int, int) {
	const factor, minPixels, maxPixels = 28.0, 56 * 56, 28 * 28 * 16384
	h := math.Max(factor, math.Round(float64(height)/factor)*factor)
	w := math.Max(factor, math.Round(float64(width)/factor)*factor)
	if h*w > maxPixels {
		beta := math.Sqrt(float64(height*width) / maxPixels)
		h = math.Max(factor, math.Floor(float64(height)/beta/factor)*factor)
		w = math.Max(factor, math.Floor(float64(width)/beta/factor)*factor)
	} else if h*w < minPixels {
		beta := math.Sqrt(minPixels / float64(height*width))
		h = math.Ceil(float64(height)*beta/factor) * factor
		w = math.Ceil(float64(width)*beta/factor) * factor
	}
	return int(w), int(h)
}

// Shrink returns the PNG with its longer side at most maxSide pixels (box
// averaged), and its size; the PNG unchanged when it is small enough or
// maxSide is 0.
func Shrink(data []byte, width, height, maxSide int) ([]byte, int, int, error) {
	if maxSide <= 0 || (width <= maxSide && height <= maxSide) {
		return data, width, height, nil
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("the screenshot is not a PNG: %w", err)
	}
	scale := float64(maxSide) / float64(max(width, height))
	w, h := max(1, int(math.Round(float64(width)*scale))), max(1, int(math.Round(float64(height)*scale)))
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+(y+1)*b.Dy()/h
		for x := 0; x < w; x++ {
			x0, x1 := b.Min.X+x*b.Dx()/w, b.Min.X+(x+1)*b.Dx()/w
			var r, g, bl, n uint32
			for yy := y0; yy < max(y1, y0+1); yy++ {
				for xx := x0; xx < max(x1, x0+1); xx++ {
					cr, cg, cb, _ := src.At(xx, yy).RGBA()
					r, g, bl, n = r+cr>>8, g+cg>>8, bl+cb>>8, n+1
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), w, h, nil
}

func (c *Client) askOllama(ctx context.Context, png []byte, prompt string) (string, error) {
	body := map[string]any{
		"model":  c.Model,
		"stream": false,
		"think":  false,
		"messages": []any{map[string]any{
			"role": "user", "content": prompt,
			"images": []string{base64.StdEncoding.EncodeToString(png)},
		}},
		"options":    map[string]any{"temperature": 0, "num_predict": 64},
		"keep_alive": c.KeepAlive,
	}
	var out struct {
		Message struct {
			Content  string `json:"content"`
			Thinking string `json:"thinking"`
		} `json:"message"`
		Error string `json:"error"`
	}
	if err := c.post(ctx, c.URL+"/api/chat", body, &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama: %s", out.Error)
	}
	// A thinking model (qwen3-vl:2b is one) thinks whatever "think" says and
	// spends the few tokens a point needs before it answers.
	if strings.TrimSpace(out.Message.Content) == "" && out.Message.Thinking != "" {
		return "", fmt.Errorf("%s only thought and gave no answer: use a model that answers directly (an -instruct tag, e.g. qwen3-vl:2b-instruct)", c.Model)
	}
	return out.Message.Content, nil
}

func (c *Client) askOpenAI(ctx context.Context, png []byte, prompt string) (string, error) {
	body := map[string]any{
		"model":       c.Model,
		"temperature": 0,
		"max_tokens":  64,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]string{
					"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)}},
				map[string]any{"type": "text", "text": prompt},
			},
		}},
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.post(ctx, c.URL+"/chat/completions", body, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("the grounding model answered nothing")
	}
	return out.Choices[0].Message.Content, nil
}

func (c *Client) post(ctx context.Context, url string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("grounding model answered %d: %.200s", resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, out)
}
