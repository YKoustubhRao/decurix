package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// HostSelection evaluates the existing model chooser on the inference host.
type HostSelection struct {
	Selection Selection `json:"selection"`
	Resources Resources `json:"resources"`
}

// EvaluateHostModel reuses the same probes, installed-model metadata and Choose
// policy as the control plane, but runs on the machine that serves inference.
func EvaluateHostModel(ctx context.Context, base string, canPull bool, current string) (HostSelection, error) {
	resources, err := ProbeResources(ctx, base, true)
	if err != nil {
		return HostSelection{}, err
	}
	installed, err := listInstalled(ctx, base)
	if err != nil {
		return HostSelection{}, err
	}
	return HostSelection{Selection: Choose(resources, installed, canPull, current), Resources: resources}, nil
}

// HostSelectionHandler only measures and recommends; it never runs inference
// or downloads/deletes models. The Windows launcher binds it to the WSL adapter.
func HostSelectionHandler(base string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\"service\":\"decurix-model-selector\"}")
	})
	mux.HandleFunc("GET /selection", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		current := r.URL.Query().Get("current")
		if len(current) > 256 {
			http.Error(w, "model name too long", http.StatusBadRequest)
			return
		}
		result, err := EvaluateHostModel(ctx, base, r.URL.Query().Get("can_pull") == "1", current)
		if err != nil {
			http.Error(w, "Could not measure the model host or list its models.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})
	return mux
}

func windowsSelection(ctx context.Context, current string) (HostSelection, error) {
	endpoint, err := url.Parse(os.Getenv("DECURIX_WINDOWS_SELECTOR_URL"))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return HostSelection{}, fmt.Errorf("Windows model selector is not configured")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/selection"
	params := endpoint.Query()
	params.Set("current", current)
	if os.Getenv("DECURIX_MODEL_AUTOPULL") == "1" {
		params.Set("can_pull", "1")
	}
	endpoint.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return HostSelection{}, err
	}
	client := &http.Client{Timeout: 35 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return HostSelection{}, fmt.Errorf("Windows model selector: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return HostSelection{}, fmt.Errorf("Windows model selector: HTTP %d", response.StatusCode)
	}
	var result HostSelection
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&result); err != nil {
		return HostSelection{}, err
	}
	if result.Resources.RAMTotalGB <= 0 || result.Resources.Cores <= 0 || result.Selection.Model.Name == "" {
		return HostSelection{}, fmt.Errorf("Windows model selector returned incomplete measurements")
	}
	return result, nil
}
