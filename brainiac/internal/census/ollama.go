package census

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ollamaURL picks the Ollama to read: the option, then OLLAMA_URL, then
// OLLAMA_HOST, then the default port on this machine.
func ollamaURL(opt string) string {
	for _, v := range []string{opt, os.Getenv("OLLAMA_URL"), os.Getenv("OLLAMA_HOST")} {
		if v = strings.TrimSpace(v); v != "" {
			return normalizeOllamaURL(v)
		}
	}
	return "http://127.0.0.1:11434"
}

// normalizeOllamaURL adds a scheme and turns a listen-all address into one a
// client can dial.
func normalizeOllamaURL(v string) string {
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil {
		return strings.TrimRight(v, "/")
	}
	host, port := u.Hostname(), u.Port()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "11434"
	}
	u.Host = net.JoinHostPort(host, port)
	return strings.TrimRight(u.String(), "/")
}

// isLoopback reports whether a URL points at this machine.
func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "0.0.0.0" || host == "::" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ollamaPool says which memory pool an Ollama's models live in.
func ollamaPool(goos, rawURL string, windowsOllama, hasWSL bool) string {
	if !isLoopback(rawURL) {
		return "remote"
	}
	if goos == "windows" {
		switch {
		case windowsOllama:
			return "host"
		case hasWSL:
			return "wsl"
		}
	}
	return "host"
}

// ollamaClient is the HTTP client for Ollama probes.
var ollamaClient = &http.Client{Timeout: 3 * time.Second}

// getJSON GETs a URL into out.
func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := ollamaClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// postJSON POSTs in to a URL and decodes the answer into out.
func postJSON(ctx context.Context, u string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ollamaClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ollamaTags is /api/tags.
type ollamaTags struct {
	Models []struct {
		Name    string `json:"name"`
		Size    uint64 `json:"size"`
		Details struct {
			Family            string `json:"family"`
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

// ollamaShow is the part of /api/show read here. It reads the manifest and
// never loads the model.
type ollamaShow struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
	Details      struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

// ollamaPS is /api/ps.
type ollamaPS struct {
	Models []struct {
		Name string `json:"name"`
		Size uint64 `json:"size"`
	} `json:"models"`
}

// probeOllama reads an Ollama's version, models and what it holds now. It
// never sends a chat or generate request: that would load a model and evict
// the one other desktops are using.
func probeOllama(ctx context.Context, base string, hasWSL bool, n *notes) *Ollama {
	var version struct {
		Version string `json:"version"`
	}
	if err := getJSON(ctx, base+"/api/version", &version); err != nil {
		n.add("no Ollama at %s (%v)", base, shortErr(err))
		return nil
	}
	o := &Ollama{URL: base, Version: version.Version}
	o.Pool = ollamaPool(runtime.GOOS, base, runtime.GOOS == "windows" && isLoopback(base) && windowsOllamaRunning(ctx), hasWSL)

	var tags ollamaTags
	if err := getJSON(ctx, base+"/api/tags", &tags); err != nil {
		n.add("Ollama at %s answered but its model list did not (%v)", base, shortErr(err))
		return o
	}
	o.Models = make([]OllamaModel, len(tags.Models))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, t := range tags.Models {
		o.Models[i] = OllamaModel{
			Name:    t.Name,
			SizeGB:  toGB(t.Size),
			Family:  t.Details.Family,
			ParamsB: parseParamSize(t.Details.ParameterSize),
			Quant:   t.Details.QuantizationLevel,
		}
		wg.Add(1)
		go func(m *OllamaModel) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var show ollamaShow
			if err := postJSON(ctx, base+"/api/show", map[string]string{"model": m.Name}, &show); err == nil {
				applyShow(m, show)
			} else {
				n.add("Ollama could not describe %s (%v)", m.Name, shortErr(err))
			}
		}(&o.Models[i])
	}
	wg.Wait()

	var ps ollamaPS
	if err := getJSON(ctx, base+"/api/ps", &ps); err == nil {
		var loaded uint64
		for _, m := range ps.Models {
			loaded += m.Size
		}
		o.LoadedGB = toGB(loaded)
	}
	return o
}

// applyShow copies what /api/show says into a model.
func applyShow(m *OllamaModel, show ollamaShow) {
	for _, c := range show.Capabilities {
		if c == "vision" {
			m.Vision = true
		}
	}
	for k, v := range show.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok {
				m.ContextLength = int(f)
			}
		}
	}
	if m.Family == "" {
		m.Family = show.Details.Family
	}
	if m.ParamsB == 0 {
		m.ParamsB = parseParamSize(show.Details.ParameterSize)
	}
	if m.Quant == "" {
		m.Quant = show.Details.QuantizationLevel
	}
}

// parseParamSize reads Ollama's parameter_size ("3.1B", "596.05M") as billions.
func parseParamSize(s string) float64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	case strings.HasSuffix(s, "M"):
		s, mult = strings.TrimSuffix(s, "M"), 0.001
	case strings.HasSuffix(s, "K"):
		s, mult = strings.TrimSuffix(s, "K"), 0.000001
	case strings.HasSuffix(s, "T"):
		s, mult = strings.TrimSuffix(s, "T"), 1000
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return round2(f * mult)
}

// shortErr keeps an error message to one short line.
func shortErr(err error) string {
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120] + "..."
	}
	return msg
}
