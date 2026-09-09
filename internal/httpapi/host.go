package httpapi

import (
	"net/http"
	"os"
	"strings"
)

// getHost derives the externally-reachable host from an incoming request, substituting the configured desktop host for localhost.
func getHost(r *http.Request) string {
	host := r.Host

	if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}

	if host == "localhost" ||
		host == "127.0.0.1" {

		envHost := os.Getenv("DESKTOP_HOST")

		if envHost != "" {
			return envHost
		}
	}

	return host
}
