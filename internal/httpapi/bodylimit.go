package httpapi

import (
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/grounder"
	"github.com/YKoustubhRao/decurix/internal/llm"
)

// defaultBodyLimit is how much of a request body any route may be sent unless bodyLimits names it: plenty for a form
// or a JSON control message, and far less than Go's own form limits (10 MiB of urlencoded, 32 MiB of multipart held in
// memory and the rest written to disk), which a caller chooses (review RW1-E-04, A-03).
const defaultBodyLimit = 1 << 20

// bodyLimits are the routes that take more than defaultBodyLimit, each with the cap its handler is built for. A path
// ending in "/" covers everything under it. The handlers keep their own, often smaller, caps; this is the outer one.
var bodyLimits = map[string]int64{
	"/api/desktops/send":              maxUploadBytes + 1<<20, // a request with attached files (limitUpload)
	"/api/desktops/record":            maxUploadBytes + 1<<20,
	"/api/desktops/ticket/attach":     maxUploadBytes + 1<<20,
	"/api/desktops/recordings/import": maxUploadBytes,          // a recording, frames included
	"/api/desktops/restore":           restoreMaxBody,          // a backup
	"/api/readiness/":                 24 << 20,                // qualification and simulation sources (qualificationJSON)
	"/api/llm/":                       llm.MaxRequestBytes + 1, // a hosted model's prompt, pictures included
	"/api/native-ollama/":             llm.MaxRequestBytes + 1, // the local model's, likewise
	"/api/grounder/locate":            grounder.MaxImageBytes*4/3 + 4096,
	"/api/agent/events":               maxAgentEventBytes, // steps with their screenshots
	"/api/outbox":                     32 << 20,           // a drafted email with its attachments
	"/api/queue":                      48 << 20,           // a work item with its files (workqueue maxBody)
	evalRunsPath:                      maxEvalUpload,      // a suite and agent trees (evalFile bounds each, unpacked)
	evalTeachPath:                     maxEvalTeachUpload, // a suite to teach one of its cases from (evalFile bounds it, unpacked)
	"/desktop/":                       maxUploadBytes,     // the desktop's own pages, proxied
	"/agent/":                         maxUploadBytes,
}

// bodyLimitFor is the outer cap on a request body for path.
func bodyLimitFor(path string) int64 {
	if n, ok := bodyLimits[path]; ok {
		return n
	}
	best, limit := 0, int64(defaultBodyLimit)
	for prefix, n := range bodyLimits {
		if strings.HasSuffix(prefix, "/") && strings.HasPrefix(path, prefix) && len(prefix) > best {
			best, limit = len(prefix), n
		}
	}
	return limit
}

// BodyLimit caps every request body at bodyLimitFor its path, before anything reads it - forms, JSON and uploads
// alike, the gated listener's sign-in form included when it wraps that listener's access middleware. Over the cap a
// handler's read fails (http.MaxBytesError), which it answers as a refusal.
func BodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, bodyLimitFor(r.URL.Path))
		}
		next.ServeHTTP(w, r)
	})
}
