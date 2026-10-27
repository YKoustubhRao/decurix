package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Found live (2026-10-14): a scanned identity document made Azure refuse the prompt, and the task ended in error.
func TestAContentFilteredRequestIsSentAgainWithoutItsPictures(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "image_url") {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"message": "The response was filtered due to the prompt triggering Azure OpenAI's content management policy."}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "DONE: read it as text"}}}})
	}))
	defer srv.Close()

	p := &OpenAIProvider{BaseURL: srv.URL, APIKey: "x", Client: srv.Client()}
	msgs := []Message{{Role: "user", Content: "office read id.png", Images: [][]byte{testPNG}, ImageLabels: []string{"id.png"}}}
	res, err := p.Chat(context.Background(), "m", msgs, Options{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "DONE: read it as text" || len(bodies) != 2 {
		t.Fatalf("got %q after %d requests", res.Text, len(bodies))
	}
	if !strings.Contains(bodies[1], "the picture of id.png was left out") {
		t.Errorf("the retry does not say what was left out: %s", bodies[1])
	}
}

// Found live (2026-10-22): bytes that are not a picture are never sent as one, and a picture the provider still
// cannot read is left out on a second try - neither ends the task.
func TestAFileThatIsNotAPictureIsNamedNotSent(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "image_url") {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"message": "You uploaded an unsupported image. Please make sure your image has of one the following formats"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}}})
	}))
	defer srv.Close()
	p := &OpenAIProvider{BaseURL: srv.URL, APIKey: "x", Client: srv.Client()}

	html := []Message{{Role: "user", Content: "office read IMG_1.jpg", Images: [][]byte{[]byte("<!DOCTYPE html><html>")},
		ImageLabels: []string{"IMG_1.jpg"}}}
	if res, err := p.Chat(context.Background(), "m", html, Options{}); err != nil || res.Text != "ok" || len(bodies) != 1 {
		t.Fatalf("res=%v err=%v requests=%d", res, err, len(bodies))
	}
	if !strings.Contains(bodies[0], "Picture 1 (IMG_1.jpg) could not be shown") {
		t.Errorf("the stand-in does not name the file: %s", bodies[0])
	}

	bodies = nil
	jpeg := []Message{{Role: "user", Content: "office read IMG_2.jpg", Images: [][]byte{{0xFF, 0xD8, 0xFF, 0x00}}}}
	if res, err := p.Chat(context.Background(), "m", jpeg, Options{}); err != nil || res.Text != "ok" || len(bodies) != 2 {
		t.Fatalf("res=%v err=%v requests=%d", res, err, len(bodies))
	}
	if !strings.Contains(bodies[0], "data:image/jpeg;base64,") || !strings.Contains(bodies[1], "could not read it as a picture") {
		t.Errorf("bodies = %q", bodies)
	}
}

func TestOtherRefusalsAreNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "invalid model"}})
	}))
	defer srv.Close()

	p := &OpenAIProvider{BaseURL: srv.URL, APIKey: "x", Client: srv.Client()}
	_, err := p.Chat(context.Background(), "m", []Message{{Role: "user", Content: "x", Images: [][]byte{{1}}}}, Options{})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want one failed call", err, calls)
	}
}

// A failure's status decides, whatever its body; whether asking again can cure it is decided by the error's type.
func TestAFailureWhoseBodyIsNotJSONIsTheFailureItIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the hosted model could not be reached", http.StatusBadGateway)
	}))
	defer srv.Close()
	p := &OpenAIProvider{BaseURL: srv.URL, APIKey: "x", Client: srv.Client()}
	_, err := p.Chat(context.Background(), "m", []Message{{Role: "user", Content: "x"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 502: the hosted model could not be reached") || !Transient(err) {
		t.Fatalf("err = %v (transient %v)", err, Transient(err))
	}
	if Transient(&HTTPError{Provider: "openai", Status: 400, Message: "invalid model"}) {
		t.Error("a refusal of the request was taken as transient")
	}
	if !Transient(&HTTPError{Status: 429}) || !Transient(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)) {
		t.Error("a rate limit or a deadline was not taken as transient")
	}
}
