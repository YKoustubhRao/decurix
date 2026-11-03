package ollama

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// EmbedWouldEvict reports whether embedding now would push a model the desktops use out of memory (C-10): the Ollama
// they use holds one model at a time and something other than the embedder is loaded. Loading the embedder then
// unloads it, and the next agent turn reloads it - minutes on a CPU host. Decided from one /api/ps answer, whichever
// chat model it is (this process's pick can differ from an older desktop's, RW1-E-03). A caller with something
// optional to embed (a suggestion) skips it then. Anything unknown counts as "would not evict": the cost is only a
// slower turn.
func EmbedWouldEvict(ctx context.Context) bool {
	if n, err := strconv.Atoi(strings.TrimSpace(defaultMaxLoadedModels())); err != nil || n != 1 {
		return false
	}
	raw := DesktopURL()
	base := raw
	if raw != BundledURL {
		base = dialableURL(ctx, raw)
	}
	var ps struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if status, err := getJSON(ctx, base+"/api/ps", 5*time.Second, 1<<20, &ps); err != nil || status != http.StatusOK {
		return false
	}
	embed := strings.TrimSuffix(EmbedModel, ":latest")
	for _, m := range ps.Models {
		if strings.TrimSuffix(m.Name, ":latest") != embed && strings.TrimSuffix(m.Model, ":latest") != embed {
			return true
		}
	}
	return false
}

// SeedCurrentModel tells model selection which chat model the existing desktops were made with (C-11), before the
// first pick of a fresh process: the pick then keeps to it unless another is clearly better (Choose's stickiness), so
// a restart with less free memory does not give new desktops a second model that the one-model Ollama swaps against
// on every turn. "" or a pick already made leaves things as they are.
func SeedCurrentModel(name string) {
	auto.mu.Lock()
	defer auto.mu.Unlock()
	if name != "" && auto.seed == "" {
		auto.seed = name
	}
}
