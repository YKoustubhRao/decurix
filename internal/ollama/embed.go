package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// embedRequest/embedResponse match Ollama's /api/embed contract: one or more
// strings in, one vector per string out, in the same order.
type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed returns the embedding vector for text from the shared Ollama
// server's embedding model (EmbedModel), reached the same way
// preloadModel reaches it - DesktopURL rewritten to whatever address this
// process itself can dial, since DesktopURL is written for a desktop
// container's point of view, not the control plane's own.
func Embed(ctx context.Context, text string) ([]float32, error) {
	baseURL := dialableURL(ctx, DesktopURL())

	body, err := json.Marshal(embedRequest{Model: EmbedModel, Input: text})
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama embed returned HTTP %d: %s", resp.StatusCode, string(detail))
	}

	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}

	if len(parsed.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama embed returned no vectors")
	}

	return parsed.Embeddings[0], nil
}
