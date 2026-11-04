package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/ollama"
)

// modelProxyTarget is the model server the desktops' proxy forwards to: the Windows app's native one
// (DECURIX_WINDOWS_OLLAMA_URL), else ollama.DesktopURL() - whatever it is: the host's own Ollama, the bundled
// container, an operator's server - with an OpenAI-compatible server's trailing /v1 left off, since the routes the
// proxy forwards carry their own (review NET-R3 R2-B1). It is the server the control plane itself warms and reaches.
func modelProxyTarget() string {
	if u := os.Getenv("DECURIX_WINDOWS_OLLAMA_URL"); u != "" {
		return u
	}
	return strings.TrimSuffix(strings.TrimRight(ollama.DesktopURL(), "/"), "/v1")
}

// nativeOllamaHandler is every desktop's only way to the local model server (desktop.DesktopModelURL): the Windows
// app's native one (whose WSL gateway changes), the bundled container, or the host's own Ollama. Only inference and
// inspection APIs are forwarded - workers cannot pull, delete or create shared models here - and a request cannot
// unload or reload the shared model for everyone (modelRequestPolicy).
func (s *Server) nativeOllamaHandler(w http.ResponseWriter, r *http.Request) {
	target, err := url.Parse(modelProxyTarget())
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		http.Error(w, "no local model server is configured", http.StatusServiceUnavailable)
		return
	}
	route := strings.TrimPrefix(r.URL.Path, "/api/native-ollama")
	allowed := (r.Method == http.MethodGet && (route == "/api/tags" || route == "/api/ps" || route == "/api/version")) ||
		(r.Method == http.MethodPost && (route == "/api/generate" || route == "/api/chat" || route == "/api/embed" || route == "/api/embeddings" || route == "/api/show")) ||
		(r.Method == http.MethodGet && route == "/v1/models") ||
		(r.Method == http.MethodPost && (route == "/v1/chat/completions" || route == "/v1/completions" || route == "/v1/embeddings"))
	if !allowed {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		if ok := applyModelPolicy(r, route); !ok {
			http.Error(w, "the model request is not a JSON object", http.StatusBadRequest)
			return
		}
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(out *http.Request) {
		out.URL.Path = route
		out.URL.RawPath = ""
		director(out)
		out.Host = target.Host
		out.Header.Del("Authorization")
		out.Header.Del("Cookie")
	}
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if os.Getenv("DECURIX_WINDOWS_OLLAMA_URL") != "" {
			http.Error(w, "Windows model server could not be reached. Open Decurix on Windows to start it.", http.StatusBadGateway)
			return
		}
		http.Error(w, "the local model server could not be reached", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

// What a desktop may ask of the shared model's residency. Ollama holds one loaded instance per model and context
// size, and a request's keep_alive and num_ctx decide that: keep_alive 0 unloads the model for every desktop, and a
// num_ctx of its own loads another instance in its place - minutes each on the target CPU host (CLAUDE.md: num_ctx
// fixed at 8192). So the proxy sets both on every native request:
//
//   - an ordinary request runs at ollama.NumCtx (8192) and holds the model for ollama.KeepAlive();
//   - one that asks for more context - TheBot's code generation asks for 32768, and a prompt cut to 8192 would be
//     silently wrong - gets exactly the one large size, maxModelCtx, and the short largeKeepAlive, so it never
//     makes a third instance and lets go of its memory soon (and with OLLAMA_MAX_LOADED_MODELS >= 2 never evicts
//     the ordinary one);
//   - an embedding request keeps its model's own context and the ordinary keep_alive.
//
// The OpenAI-compatible routes carry neither field, and are passed as they are.
const largeKeepAlive = "5m"

// maxModelCtx is the one context size above ollama.NumCtx a desktop may ask for: DECURIX_MODEL_MAX_CTX, 32768 by
// default (TheBot's code generation window).
func maxModelCtx() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("DECURIX_MODEL_MAX_CTX"))); err == nil && n >= ollama.NumCtx {
		return n
	}
	return 32768
}

// applyModelPolicy rewrites a POST to a native route by modelRequestPolicy, in place. It reports false for a body
// that is not a JSON object.
func applyModelPolicy(r *http.Request, route string) bool {
	switch route {
	case "/api/generate", "/api/chat", "/api/embed", "/api/embeddings":
	default:
		return true
	}
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return false
	}
	out, ok := modelRequestPolicy(raw, route)
	if !ok {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return true
}

// modelRequestPolicy is body with keep_alive and options.num_ctx set as the comment above says; false when body is
// not a JSON object.
func modelRequestPolicy(body []byte, route string) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil || req == nil {
		return nil, false
	}
	opts, _ := req["options"].(map[string]any)
	if route == "/api/embed" || route == "/api/embeddings" {
		if opts != nil {
			delete(opts, "num_ctx")
		}
		req["keep_alive"] = ollama.KeepAlive()
	} else {
		if opts == nil {
			opts = map[string]any{}
			req["options"] = opts
		}
		asked := 0
		if n, ok := opts["num_ctx"].(json.Number); ok {
			if v, err := n.Int64(); err == nil {
				asked = int(v)
			}
		}
		if asked > ollama.NumCtx {
			opts["num_ctx"] = maxModelCtx()
			req["keep_alive"] = largeKeepAlive
		} else {
			opts["num_ctx"] = ollama.NumCtx
			req["keep_alive"] = ollama.KeepAlive()
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return nil, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}
