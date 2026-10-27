package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureServer answers every request with reply and keeps the last body.
func captureServer(t *testing.T, reply string) (*httptest.Server, *string, *string) {
	t.Helper()
	var body, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, path = string(raw), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &body, &path
}

// testPNG is the start of a PNG file: enough for a provider to be sent it as a picture (ImageType).
var testPNG = []byte("\x89PNG\r\n\x1a\nIHDR")

var withPicture = []Message{{Role: "user", Content: "what is on screen?", Images: [][]byte{testPNG}}}

var pictureB64 = base64.StdEncoding.EncodeToString(testPNG)

func TestEveryProviderSendsPicturesInItsOwnShape(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		srv, body, _ := captureServer(t, `{"choices":[{"message":{"content":"a form"}}]}`)
		p := NewOpenAI("k", "")
		p.BaseURL = srv.URL
		if _, err := p.Chat(context.Background(), "m", withPicture, Options{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(*body, `"type":"image_url"`) || !strings.Contains(*body, "data:image/png;base64,"+pictureB64) {
			t.Errorf("body = %s", *body)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		srv, body, _ := captureServer(t, `{"content":[{"type":"text","text":"a form"}]}`)
		p := NewAnthropic("k", "m")
		p.BaseURL = srv.URL
		if _, err := p.Chat(context.Background(), "m", withPicture, Options{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(*body, `"type":"image"`) || !strings.Contains(*body, `"data":"`+pictureB64+`"`) {
			t.Errorf("body = %s", *body)
		}
	})

	t.Run("ollama", func(t *testing.T) {
		srv, body, _ := captureServer(t, `{"message":{"role":"assistant","content":"a form"},"done":true}`)
		p := NewOllama(srv.URL)
		if _, err := p.Chat(context.Background(), "m", withPicture, Options{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(*body, `"images":["`+pictureB64+`"]`) {
			t.Errorf("body = %s", *body)
		}
	})

	t.Run("text stays a string", func(t *testing.T) {
		srv, body, _ := captureServer(t, `{"choices":[{"message":{"content":"hi"}}]}`)
		p := NewOpenAI("k", "")
		p.BaseURL = srv.URL
		if _, err := p.Chat(context.Background(), "m", []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
			t.Fatal(err)
		}
		var sent struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(*body), &sent)
		if _, ok := sent.Messages[0].Content.(string); !ok {
			t.Errorf("a message without pictures was not sent as plain text: %s", *body)
		}
	})
}

func TestTheLocalModelIsAskedWhatItCanDo(t *testing.T) {
	srv, body, path := captureServer(t, `{"capabilities":["completion","vision"],"model_info":{"qwen25vl.context_length":128000}}`)

	vision, window, err := ProbeOllama(context.Background(), srv.URL, "qwen2.5vl:3b")
	if err != nil || !vision || window != 128000 {
		t.Fatalf("probe = %v %d %v", vision, window, err)
	}
	// /api/show reads the manifest - never a chat, which would evict the
	// prompt every desktop's next turn has cached.
	if *path != "/api/show" || !strings.Contains(*body, "qwen2.5vl:3b") {
		t.Errorf("asked %s with %s", *path, *body)
	}

	b := NewBrain(Capabilities{Brain: "ollama"})
	b.LearnFromOllama(context.Background(), srv.URL, "qwen2.5vl:3b", false, func(string, ...any) {})
	if c := b.Caps(); !c.Vision || c.ContextTokens != 128000 || c.Hosted {
		t.Errorf("learned caps = %+v", c)
	}

	// Told otherwise by the control plane, vision is not overridden.
	told := NewBrain(Capabilities{Brain: "ollama", Vision: false})
	told.LearnFromOllama(context.Background(), srv.URL, "m", true, func(string, ...any) {})
	if told.Caps().Vision {
		t.Error("a probe overrode what the control plane said")
	}
}

func TestBudgetsFollowTheBrain(t *testing.T) {
	var nilBrain *Brain
	local := nilBrain.Caps()
	hosted := Capabilities{Hosted: true}
	if local.CommandOutputChars() >= hosted.CommandOutputChars() ||
		local.ScreenTextChars() >= hosted.ScreenTextChars() ||
		local.OperatorTurns() >= hosted.OperatorTurns() ||
		local.ScreenshotScale() <= hosted.ScreenshotScale() {
		t.Errorf("local %+v vs hosted %+v budgets", local, hosted)
	}
	if local.ScreenTextChars() > local.CommandOutputChars() {
		t.Error("a local model's element list would not fit in what it is shown of a command's output")
	}
}
