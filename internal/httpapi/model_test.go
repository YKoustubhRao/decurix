package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/ollama"
)

// GET /api/model reports the machine's resources and the model that fits it,
// measured against a real /proc and an Ollama that lists what it has.
func TestModelHandlerReportsTheFit(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[
				{"name":"qwen2.5-coder:1.5b","size":1000000000,"details":{"family":"qwen2","parameter_size":"1.5B"}},
				{"name":"nomic-embed-text:latest","size":274000000,"details":{"family":"nomic-bert","parameter_size":"137M"}}]}`))
		default:
			w.Write([]byte(`{"models":[]}`))
		}
	}))
	defer fake.Close()

	t.Setenv("OLLAMA_URL", fake.URL)
	t.Setenv("OLLAMA_MODEL", "")

	rec := httptest.NewRecorder()
	// The route answers from the last measurement and never waits for one (ollama.CurrentStatus): measured first.
	ollama.RefreshStatus(context.Background())
	(&Server{}).modelHandler(rec, httptest.NewRequest(http.MethodGet, "/api/model", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	var got struct {
		Mode      string `json:"mode"`
		Model     string `json:"model"`
		Best      string `json:"best"`
		Resources struct {
			RAMTotalGB float64 `json:"ram_total_gb"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Mode != "auto" || got.Model == "" || got.Best == "" || got.Resources.RAMTotalGB <= 0 {
		t.Errorf("unexpected status: %s", rec.Body)
	}
	if got.Best == "nomic-embed-text:latest" {
		t.Error("an embedding model was offered as the chat model")
	}
}

// A model somebody named is reported as pinned, with the best fit beside it.
func TestModelHandlerReportsAPinnedModel(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("OLLAMA_MODEL", "llama3.2:1b")

	rec := httptest.NewRecorder()
	// The route answers from the last measurement and never waits for one (ollama.CurrentStatus): measured first.
	ollama.RefreshStatus(context.Background())
	(&Server{}).modelHandler(rec, httptest.NewRequest(http.MethodGet, "/api/model", nil))

	var got struct{ Mode, Model string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "pinned" || got.Model != "llama3.2:1b" {
		t.Errorf("got %+v, want pinned llama3.2:1b", got)
	}
}

func TestModelHandlerIsGetOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).modelHandler(rec, httptest.NewRequest(http.MethodPost, "/api/model", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", rec.Code)
	}
}
