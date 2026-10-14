package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each picture's label goes right before it: two scans in one message must not be told apart by order alone.
func TestOpenAIPutsEachPicturesLabelRightBeforeIt(t *testing.T) {
	var parts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.Unmarshal(body.Messages[0].Content, &parts)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}}})
	}))
	defer srv.Close()

	p := NewOpenAI("k", "")
	p.BaseURL = srv.URL
	msg := Message{Role: "user", Content: "two scans", Images: [][]byte{[]byte("a"), []byte("b")},
		ImageLabels: []string{"scan_0030.pdf, page 1", "scan_0031.pdf, page 1"}}
	if _, err := p.Chat(context.Background(), "m", []Message{msg}, Options{}); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, part := range parts {
		if part["type"] == "text" {
			kinds = append(kinds, "text:"+part["text"].(string))
		} else {
			kinds = append(kinds, "image")
		}
	}
	want := []string{"text:two scans", "text:Picture 1: scan_0030.pdf, page 1", "image", "text:Picture 2: scan_0031.pdf, page 1", "image"}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Fatalf("parts = %v\nwant    %v", kinds, want)
	}
}

func TestAProviderWithoutInterleavingListsTheLabels(t *testing.T) {
	m := Message{Content: "x", Images: [][]byte{{1}, {2}}, ImageLabels: []string{"a.pdf, page 1", "b.pdf, page 1"}}
	if got := m.LabelledContent(); !strings.Contains(got, "1) a.pdf, page 1; 2) b.pdf, page 1") {
		t.Fatalf("LabelledContent = %q", got)
	}
	if got := (Message{Content: "x", Images: [][]byte{{1}}}).LabelledContent(); got != "x" {
		t.Fatalf("unlabelled = %q, want the content unchanged", got)
	}
}
