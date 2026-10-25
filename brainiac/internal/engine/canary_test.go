package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
)

// quizOllama answers the canary's questions the way a working model would;
// blind answers pictures without looking, mumble answers nothing usable.
type quizOllama struct {
	blind, mumble bool
	images        int
}

func (q *quizOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/ps":
		fmt.Fprint(w, `{"models":[]}`)
	case "/api/chat":
		var body struct {
			Messages []struct {
				Content string   `json:"content"`
				Images  []string `json:"images"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		last := body.Messages[len(body.Messages)-1]
		q.images += len(last.Images)
		answer := "I am not sure."
		switch {
		case q.mumble:
			answer = "<think>"
		case strings.Contains(last.Content, "17 + 25"):
			answer = "42"
		case strings.Contains(last.Content, "ready"):
			answer = "ready"
		case strings.Contains(last.Content, "picture") && len(last.Images) > 0 && !q.blind:
			answer = "4417"
		case strings.Contains(last.Content, "12 multiplied"):
			answer = "12 x 13 = 156. The answer is 156."
		}
		fmt.Fprintf(w, "{\"message\":{\"content\":%q},\"done\":true}\n", answer)
	default:
		http.NotFound(w, r)
	}
}

func canaryEngine(t *testing.T, q *quizOllama) *Engine {
	t.Helper()
	srv := httptest.NewServer(q)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL, cfg.Web = config.ModeLocal, srv.URL, config.WebOff
	return newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "vlm", "vlm:2b"), ollamaPick(catalog.RoleVision, "vlm", "vlm:2b"),
		ollamaPick("math", "mathy", "mathy:1b")))
}

func TestCanaryPassesModelsThatWork(t *testing.T) {
	q := &quizOllama{}
	e := canaryEngine(t, q)
	if err := e.Canary(context.Background(), []string{catalog.RoleBrain, catalog.RoleVision, "math"}); err != nil {
		t.Fatal(err)
	}
	if q.images != 1 {
		t.Fatalf("%d pictures sent, want the one for vision", q.images)
	}
}

func TestCanaryCatchesABlindOrBrokenModel(t *testing.T) {
	var ce *CanaryError
	err := canaryEngine(t, &quizOllama{blind: true}).Canary(context.Background(), []string{catalog.RoleBrain, catalog.RoleVision})
	if !errors.As(err, &ce) || ce.Role != catalog.RoleVision || ce.Model != "vlm" {
		t.Fatalf("blind: %v", err)
	}
	err = canaryEngine(t, &quizOllama{mumble: true}).Canary(context.Background(), []string{catalog.RoleBrain})
	if !errors.As(err, &ce) || ce.Role != catalog.RoleBrain || !strings.Contains(err.Error(), "42") {
		t.Fatalf("mumble: %v", err)
	}
}

func TestCanaryPictureAndSilence(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(digitsPNG("4417")))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 24*2+4*6*12 || b.Dy() != 24*2+7*12 {
		t.Fatalf("picture %v", b)
	}
	if w := silenceWAV(1); len(w) != 44+32000 || string(w[:4]) != "RIFF" || string(w[8:12]) != "WAVE" {
		t.Fatalf("wav header %q, %d bytes", w[:12], len(w))
	}
}
