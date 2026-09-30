package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

// The KV cache a model holds at NumCtx is worked out from its own
// architecture, not guessed from its parameter count: per token it is
// layers x KV heads x (key + value width) x bytes per element. The KV head
// count is what matters, not the query heads - grouped-query attention makes
// them differ a lot, and a mixture-of-experts model's cache follows its
// attention, not its total parameters (qwen3:30b holds less per token than a
// dense 14B). Following no-mans-code/ollama-kv-profiler's hardware.rs.
//
// It is read from /api/show, which reads the manifest and never loads the
// model - safe to ask the shared Ollama (see the memory-starved host note).

// kvCache remembers each model's bytes per token (by digest, else name), so a
// model is asked about once per control-plane run.
var kvCache struct {
	mu sync.Mutex
	m  map[string]float64
}

// kvElementBytes is the size of one cached key/value element for the cache
// type the Ollama runs with. OLLAMA_KV_CACHE_TYPE is the Ollama server's own
// setting; the control plane gives the bundled one q8_0, and assumes the same
// of a server named in .env unless told otherwise.
func kvElementBytes() float64 {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OLLAMA_KV_CACHE_TYPE"))) {
	case "f16", "fp16", "f32":
		return 2
	case "q4_0":
		return 0.5
	}

	return 1 // q8_0, the default here
}

// installedKVBytes returns the KV cache bytes per token (at one byte per
// element) of the model name, asking the Ollama at baseURL once; 0 when it
// cannot be worked out.
func installedKVBytes(ctx context.Context, baseURL, name, digest string) float64 {
	key := digest
	if key == "" {
		key = name
	}

	kvCache.mu.Lock()
	v, ok := kvCache.m[key]
	kvCache.mu.Unlock()
	if ok {
		return v
	}

	v, err := fetchKVBytes(ctx, baseURL, name)
	if err != nil {
		return 0 // not cached: a later evaluation asks again
	}

	kvCache.mu.Lock()
	if kvCache.m == nil {
		kvCache.m = map[string]float64{}
	}
	kvCache.m[key] = v
	kvCache.mu.Unlock()

	return v
}

// fetchKVBytes asks /api/show for the model's architecture.
func fetchKVBytes(ctx context.Context, baseURL, name string) (float64, error) {
	body, _ := json.Marshal(map[string]string{"model": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/show", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil // no such model, or an Ollama without model_info: fall back
	}

	var show struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&show); err != nil {
		return 0, err
	}

	return kvBytesPerToken(show.ModelInfo), nil
}

// kvBytesPerToken computes KV cache bytes per token, per byte of element, from
// /api/show's model_info; 0 when a needed field is missing (an architecture
// that caches differently, such as DeepSeek's latent attention, is left to the
// estimate). Head counts may be one number or one per layer: Gemma's
// interleaved local/global attention lists [8,8,8,8,8,2,...], and reading that
// as a scalar is how the profiler once overstated a model's cache by 2x.
func kvBytesPerToken(info map[string]any) float64 {
	arch, _ := info["general.architecture"].(string)
	if arch == "" {
		return 0
	}

	field := func(k string) any { return info[arch+"."+k] }
	layers := number(field("block_count"))
	if layers <= 0 {
		return 0
	}

	kvHeads := perLayer(field("attention.head_count_kv"), int(layers))
	heads := perLayer(field("attention.head_count"), int(layers))
	if kvHeads == nil {
		kvHeads = heads // no grouped-query attention: every head is cached
	}
	if kvHeads == nil {
		return 0
	}

	keyLen := number(field("attention.key_length"))
	valLen := number(field("attention.value_length"))
	if keyLen <= 0 || valLen <= 0 {
		embed := number(field("embedding_length"))
		if embed <= 0 || heads == nil {
			return 0
		}
		dim := embed / average(heads)
		if keyLen <= 0 {
			keyLen = dim
		}
		if valLen <= 0 {
			valLen = dim
		}
	}

	var total float64
	for _, h := range kvHeads {
		total += h * (keyLen + valLen)
	}

	return total
}

// perLayer reads a head count given as one number or one per layer, as one
// value per layer; nil when absent.
func perLayer(v any, layers int) []float64 {
	if list, ok := v.([]any); ok {
		out := make([]float64, 0, len(list))
		for _, x := range list {
			out = append(out, number(x))
		}
		if len(out) == 0 {
			return nil
		}

		return out
	}

	n := number(v)
	if n <= 0 {
		return nil
	}

	out := make([]float64, layers)
	for i := range out {
		out[i] = n
	}

	return out
}

// number reads a JSON number, or 0.
func number(v any) float64 {
	f, _ := v.(float64)

	return f
}

// average is the mean of xs, ignoring zeros (layers with no attention).
func average(xs []float64) float64 {
	var sum, n float64
	for _, x := range xs {
		if x > 0 {
			sum += x
			n++
		}
	}
	if n == 0 {
		return 1
	}

	return sum / n
}
