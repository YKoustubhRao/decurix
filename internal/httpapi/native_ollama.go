package httpapi

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/ollama"
)

// modelProxyTarget is the model server the desktops' proxy forwards to: the Windows app's native one
// (DECURIX_WINDOWS_OLLAMA_URL), else the bundled container when that is the desktops' model server (OLLAMA_URL is
// ollama.BundledURL), else none ("").
func modelProxyTarget() string {
	if u := os.Getenv("DECURIX_WINDOWS_OLLAMA_URL"); u != "" {
		return u
	}
	if ollama.DesktopURL() == ollama.BundledURL {
		return ollama.BundledURL
	}
	return ""
}

// nativeOllamaHandler gives Linux workers a stable control-plane URL for the local model server: the Windows app's
// native one (whose WSL gateway changes) or the bundled container, which no desktop network carries (review NET-R3
// B3). Only inference/inspection APIs are forwarded; workers cannot pull, delete or create shared models here.
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
