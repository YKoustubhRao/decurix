package grounder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseReadsEveryShapeModelsAnswerIn(t *testing.T) {
	cases := map[string][2]float64{
		`{"point_2d": [512, 300]}`:                                      {512, 300},
		"```json\n[{\"point_2d\": [10, 20], \"label\": \"Save\"}]\n```": {10, 20},
		`{"bbox_2d": [100, 200, 300, 400]}`:                             {200, 300},
		`(640, 360)`:                                                    {640, 360},
		`<think>it is near (1, 1)</think>(77.5, 88)`:                    {77.5, 88},
		`click(start_box='(120,45)')`:                                   {120, 45},
		`The button is at x=33, y=44.`:                                  {33, 44},
	}
	for answer, want := range cases {
		x, y, ok := Parse(answer)
		if !ok || x != want[0] || y != want[1] {
			t.Errorf("Parse(%q) = %v,%v,%v; want %v", answer, x, y, ok, want)
		}
	}
	for _, answer := range []string{"", "I cannot see it.", "[5]"} {
		if _, _, ok := Parse(answer); ok {
			t.Errorf("Parse(%q) found a point", answer)
		}
	}
}

func TestToPixelsPerFamily(t *testing.T) {
	if x, y := ToPixels(CoordsNorm1000, 500, 500, 1280, 720); x != 640 || y != 360 {
		t.Errorf("norm1000 centre = %d,%d", x, y)
	}
	// 1280x720 is resized to 1288x728 by Qwen2.5-VL: 1288,728 is the far corner.
	if w, h := SmartResize(1280, 720); w != 1288 || h != 728 {
		t.Fatalf("SmartResize = %dx%d", w, h)
	}
	if x, y := ToPixels(CoordsQwen25, 644, 364, 1280, 720); x != 640 || y != 360 {
		t.Errorf("qwen25 centre = %d,%d", x, y)
	}
	if x, y := ToPixels(CoordsPixels, 5000, -3, 1280, 720); x != 1279 || y != 0 {
		t.Errorf("a point off the image is not kept inside it: %d,%d", x, y)
	}
}

func TestCoordsForKnownFamilies(t *testing.T) {
	for model, want := range map[string]string{
		"qwen3-vl:2b": CoordsNorm1000, "osunlp/UGround-V1-7B": CoordsNorm1000,
		"Salesforce/GTA1-7B": CoordsQwen25, "Hcompany/Holo1.5-7B": CoordsQwen25,
		"ByteDance-Seed/UI-TARS-1.5-7B": CoordsQwen25, "qwen2.5vl:7b": CoordsQwen25,
	} {
		if got := CoordsFor(model); got != want {
			t.Errorf("CoordsFor(%q) = %s, want %s", model, got, want)
		}
	}
}

func TestLocateThroughOllama(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"message":{"content":"{\"point_2d\": [250, 750]}"}}`))
	}))
	defer srv.Close()

	c := &Client{API: "ollama", URL: srv.URL, Model: "qwen3-vl:2b", Coords: CoordsNorm1000, Timeout: 5e9}
	p, err := c.Locate(context.Background(), []byte("png"), 1280, 720, "the  Save\nbutton")
	if err != nil {
		t.Fatal(err)
	}
	if p.X != 320 || p.Y != 540 {
		t.Errorf("point %d,%d", p.X, p.Y)
	}
	msg := got["messages"].([]any)[0].(map[string]any)
	if !strings.Contains(msg["content"].(string), "Description: the Save button") || len(msg["images"].([]any)) != 1 {
		t.Errorf("request %v", msg)
	}
}

func TestLocateThroughOpenAICompatibleServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("%s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"(644, 364)"}}]}`))
	}))
	defer srv.Close()

	c := &Client{API: "openai", URL: srv.URL + "/v1", Key: "k", Model: "GTA1-7B", Coords: CoordsQwen25, Timeout: 5e9}
	p, err := c.Locate(context.Background(), []byte("png"), 1280, 720, "Save")
	if err != nil || p.X != 640 || p.Y != 360 {
		t.Errorf("%v %+v", err, p)
	}
}

func TestNotConfigured(t *testing.T) {
	t.Setenv("DECURIX_GROUNDER_MODEL", "")
	if FromEnv("http://x") != nil {
		t.Error("a client with no model")
	}
	var c *Client
	if _, err := c.Locate(context.Background(), nil, 1, 1, "x"); err != ErrNotConfigured {
		t.Error(err)
	}
	if c.Status()["configured"] != false {
		t.Error("status")
	}
}

func TestAShrunkPictureGivesScreenPixels(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	var buf bytes.Buffer
	png.Encode(&buf, img)

	var sent image.Config
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Images []string `json:"images"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.StdEncoding.DecodeString(body.Messages[0].Images[0])
		sent, _ = png.DecodeConfig(bytes.NewReader(raw))
		// 100,50 of the 640x360 picture sent.
		w.Write([]byte(`{"message":{"content":"(100, 50)"}}`))
	}))
	defer srv.Close()

	// A generous budget: the shrink and the local round trip are quick, but a loaded 8 GB host once took over 10 s (W-21).
	c := &Client{API: "ollama", URL: srv.URL, Model: "m", Coords: CoordsPixels, MaxSide: 640, Timeout: 60e9}
	p, err := c.Locate(context.Background(), buf.Bytes(), 1280, 720, "x")
	if err != nil {
		t.Fatal(err)
	}
	if sent.Width != 640 || sent.Height != 360 {
		t.Errorf("sent %dx%d", sent.Width, sent.Height)
	}
	if p.X != 200 || p.Y != 100 {
		t.Errorf("point %d,%d, want 200,100", p.X, p.Y)
	}
}

func TestAThinkingModelIsNamedAsTheProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"message":{"content":"","thinking":"Okay, the user wants"}}`))
	}))
	defer srv.Close()
	c := &Client{API: "ollama", URL: srv.URL, Model: "qwen3-vl:2b", Coords: CoordsNorm1000, Timeout: 5e9}
	_, err := c.Locate(context.Background(), []byte("png"), 1280, 720, "x")
	if err == nil || !strings.Contains(err.Error(), "-instruct") {
		t.Errorf("err %v", err)
	}
}
