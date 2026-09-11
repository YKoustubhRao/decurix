package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// Embedder is a separate, narrower interface from Provider - embedding is
// a distinct concern from chat completion, and the Data RAG only ever
// needs this, not the full chat surface.
type Embedder interface {
	Embed(ctx context.Context, model string, text string) ([]float32, error)
}

type ollamaEmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

// Embed calls Ollama's real /api/embed endpoint - one real HTTP call, one
// real vector back, never a fabricated/zero vector on partial failure
// (an error is returned instead, so a caller never silently indexes junk).
func (o *OllamaProvider) Embed(ctx context.Context, model string, text string) ([]float32, error) {
	if model == "" {
		model = o.EmbedModel
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	body, err := json.Marshal(ollamaEmbedRequest{Model: model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("ollama embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var parsed ollamaEmbedResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("ollama embed: parse response: %w (body: %s)", err, string(respBytes))
	}
	if parsed.Error != "" {
		return nil, fmt.Errorf("ollama embed error: %s", parsed.Error)
	}
	if len(parsed.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama embed: response contained no embeddings")
	}
	return parsed.Embeddings[0], nil
}

// CosineSimilarity is real vector math, not a stand-in - used for
// brute-force ranking over the Data RAG's stored chunks.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
