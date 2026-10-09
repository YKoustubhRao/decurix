package httpapi

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

// nativeOllamaHandler gives Linux workers a stable control-plane URL even
// when WSL's Windows gateway changes. Only inference/inspection APIs are
// forwarded; workers cannot pull, delete or create shared models here.
func (s *Server) nativeOllamaHandler(w http.ResponseWriter, r *http.Request) {
	target, err := url.Parse(os.Getenv("DECURIX_WINDOWS_OLLAMA_URL"))
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		http.Error(w, "native Windows Ollama is not configured", http.StatusServiceUnavailable)
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
		http.Error(w, "Windows model server could not be reached. Open Decurix on Windows to start it.", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
