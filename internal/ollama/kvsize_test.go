package ollama

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// qwen2.5-coder:3b's architecture as /api/show reports it: 36 layers, 16
// query heads but only 2 KV heads, width 2048 (head dim 128).
func qwen25Coder3B() map[string]any {
	return map[string]any{
		"general.architecture":          "qwen2",
		"qwen2.block_count":             36.0,
		"qwen2.embedding_length":        2048.0,
		"qwen2.attention.head_count":    16.0,
		"qwen2.attention.head_count_kv": 2.0,
	}
}

// The KV head count sets the cache, not the query head count:
// 36 x 2 x (128 + 128) elements, 36 KB per token at f16 - the figure the
// profiler measured for this model.
func TestKVBytesPerTokenUsesKVHeads(t *testing.T) {
	if got := kvBytesPerToken(qwen25Coder3B()); got != 18432 {
		t.Fatalf("got %v elements/token, want 18432", got)
	}
}

// A head count given per layer (Gemma's interleaved local/global attention)
// is summed layer by layer, never read as one number.
func TestKVBytesPerTokenReadsPerLayerHeadCounts(t *testing.T) {
	var kv []any
	for i := 0; i < 5; i++ {
		kv = append(kv, 8.0, 8.0, 8.0, 8.0, 8.0, 2.0)
	}
	info := map[string]any{
		"general.architecture":           "gemma3",
		"gemma3.block_count":             30.0,
		"gemma3.embedding_length":        2816.0,
		"gemma3.attention.head_count":    16.0,
		"gemma3.attention.head_count_kv": kv,
		"gemma3.attention.key_length":    256.0,
		"gemma3.attention.value_length":  256.0,
	}

	// 5 x (5 x 8 + 2) = 210 KV heads over all layers, 512 elements each.
	if got := kvBytesPerToken(info); got != 210*512 {
		t.Fatalf("got %v, want %v", got, 210*512)
	}
}

// An explicit key/value width wins over embedding/heads: Qwen3's MoE models
// have head dim 128 on a 2048-wide model with 32 query heads, which the
// division would put at 64.
func TestKVBytesPerTokenPrefersExplicitKeyLength(t *testing.T) {
	info := map[string]any{
		"general.architecture":             "qwen3moe",
		"qwen3moe.block_count":             48.0,
		"qwen3moe.embedding_length":        2048.0,
		"qwen3moe.attention.head_count":    32.0,
		"qwen3moe.attention.head_count_kv": 4.0,
		"qwen3moe.attention.key_length":    128.0,
		"qwen3moe.attention.value_length":  128.0,
	}
	if got := kvBytesPerToken(info); got != 48*4*256 {
		t.Fatalf("got %v, want %v", got, 48*4*256)
	}
}

// What cannot be worked out is 0, and the estimate is used instead.
func TestKVBytesPerTokenUnknownIsZero(t *testing.T) {
	for _, info := range []map[string]any{
		{},
		{"general.architecture": "deepseek2", "deepseek2.block_count": 27.0},
		{"general.architecture": "llama", "llama.attention.head_count_kv": 8.0},
	} {
		if got := kvBytesPerToken(info); got != 0 {
			t.Errorf("%v: got %v, want 0", info, got)
		}
	}
}

// A known cache replaces the size-based guess, and the cache type counts.
func TestNeedGBUsesTheKnownKVCache(t *testing.T) {
	t.Setenv("OLLAMA_KV_CACHE_TYPE", "f16")
	m := Candidate{Name: "x", SizeGB: 9, ParamsB: 14.8, KVPerToken: 48 * 8 * 256}
	kv := float64(48*8*256) * 2 * NumCtx / gb
	want := 9 + 0.5 + 0.02*14.8 + kv
	if got := m.NeedGB(); math.Abs(got-want) > 1e-9 {
		t.Fatalf("NeedGB = %v, want %v", got, want)
	}

	t.Setenv("OLLAMA_KV_CACHE_TYPE", "")
	if q8 := m.NeedGB(); math.Abs((want-q8)-kv/2) > 1e-9 {
		t.Fatalf("q8_0 should halve the cache: f16 %v, q8_0 %v", want, q8)
	}

	m.KVPerToken = 0
	if got := m.NeedGB(); got != 9+0.5+0.06*14.8 {
		t.Fatalf("unknown cache should keep the estimate, got %v", got)
	}
}

// listInstalled asks /api/show once per model and never chats with it.
func TestListInstalledReadsArchitectureOnce(t *testing.T) {
	kvCache.mu.Lock()
	kvCache.m = nil
	kvCache.mu.Unlock()

	shows := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"qwen2.5-coder:3b","size":1900000000,"digest":"abc",`+
				`"details":{"family":"qwen2","parameter_size":"3.1B"}}]}`)
		case "/api/show":
			shows++
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "qwen2.5-coder:3b") {
				t.Errorf("show asked about %s", body)
			}
			json.NewEncoder(w).Encode(map[string]any{"model_info": qwen25Coder3B()})
		default:
			t.Errorf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	for i := 0; i < 2; i++ {
		got, err := listInstalled(context.Background(), srv.URL)
		if err != nil || len(got) != 1 {
			t.Fatalf("listInstalled = %v, %v", got, err)
		}
		if got[0].KVPerToken != 18432 {
			t.Fatalf("KVPerToken = %v, want 18432", got[0].KVPerToken)
		}
	}
	if shows != 1 {
		t.Fatalf("/api/show called %d times, want 1", shows)
	}
}
