package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
)

// jsonResponse writes a value to the response writer as JSON.
func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	_ = json.NewEncoder(w).Encode(v)
}

// jsonError writes an error to the response writer as a JSON-formatted HTTP 500 response.
func jsonError(w http.ResponseWriter, err error) {
	log.Println("jsonError:", err)

	// A request naming a desktop that does not exist, or input a store
	// rejected, is the caller's mistake - not an internal error.
	if errors.Is(err, desktop.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "desktop not found")
		return
	}

	if errors.Is(err, desktop.ErrReservedName) {
		jsonStatusError(w, http.StatusBadRequest, "Names starting with practice- or readiness- are kept for practice and readiness runs. Pick another name.")
		return
	}

	if errors.Is(err, desktop.ErrNoFence) {
		jsonStatusError(w, http.StatusServiceUnavailable, "Desktops are not started while the network fence that keeps them apart is not in place. Restart Decurix; if this stays, the app's Linux host could not add its firewall rules.")
		return
	}

	if errors.Is(err, desktop.ErrSubnetsOutsideFence) {
		jsonStatusError(w, http.StatusServiceUnavailable, "DECURIX_DESKTOP_SUBNETS names a range this computer's network fence does not cover. Remove it from your settings, or keep it inside 10.213.0.0/16.")
		return
	}

	// No room to start a desktop: the text is the control plane's own ("not enough memory: N desktops are busy"),
	// never Docker's (idlepause.MakeRoom, docs/lightweight.md L2).
	var noRoom *desktop.NoRoomError
	if errors.As(err, &noRoom) {
		jsonStatusError(w, http.StatusServiceUnavailable, refusal(noRoom))
		return
	}

	// A delete refused while runs of the desktop are not in the run record yet: the person may try again, or send it
	// again with force (held marks the case the dashboard offers "Delete anyway" on).
	var unsaved *unsavedRunsError
	if errors.As(err, &unsaved) {
		jsonResponseStatus(w, http.StatusConflict, map[string]any{"error": refusal(unsaved), "held": true, "unsaved_runs": unsaved.n})
		return
	}

	if errors.Is(err, errForceNeedsPerson) {
		jsonStatusError(w, http.StatusForbidden, refusal(errForceNeedsPerson))
		return
	}

	if errors.Is(err, memory.ErrInvalid) {
		jsonStatusError(w, http.StatusBadRequest, "invalid request")
		return
	}

	jsonStatusError(
		w,
		http.StatusInternalServerError,
		"internal server error",
	)
}

// internalError answers a failure that is the server's own (a store, Docker, a template): the detail is logged under
// what, and the client gets a fixed 500 (the CLAUDE.md rule; TestNoResponseEchoesAnError).
func internalError(w http.ResponseWriter, what string, err error) {
	log.Printf("%s: %v", what, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// badForm answers a form body that could not be read: 413 over its cap, 400 otherwise, never the parser's words.
func badForm(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "the form could not be read", http.StatusBadRequest)
}

// refusal is the text of an error this server made for the person to read - a validation's reason, a refusal with a
// way forward (an InputError, errBrain, a shared folder's or a backup's reason) - never one a store, Docker or the
// network returned. The name is the review point: only such an error may be passed to it.
func refusal(err error) string { return err.Error() }

// refusalLines is the lines of a refusal made of several reasons this server's own code wrote for the person (a
// suite's problems, eval.Problems), each one theirs to read. The same review point as refusal.
func refusalLines(lines []string) []string { return lines }

// providerSaid is what the person's own account (a model provider, a mail server) answered when they set it up -
// "invalid x-api-key", "deployment not found" - which is what they need to fix the form.
func providerSaid(err error) string { return err.Error() }

// viewerReason is why a host app's viewer could not do what the person asked (no field there, the app closed), one
// short line for the viewer. The helper's words describe the app on screen, not the control plane.
func viewerReason(err error) string {
	line := strings.Join(strings.Fields(err.Error()), " ")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// maxJSONBody bounds a small JSON control message - a persona, a resource request, a host-bridge command. A route
// that takes more (an upload, a hosted model's prompt) sets its own cap; none reads a body unbounded (A-03).
const maxJSONBody = 64 << 10

// limitJSON caps r's body at maxJSONBody, before anything reads it.
func limitJSON(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
}

// badJSON answers a JSON body that did not decode: 413 when it ran over its cap, 400 otherwise. Neither echoes the
// decoder's words.
func badJSON(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		jsonStatusError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	jsonStatusError(w, http.StatusBadRequest, "invalid json payload")
}

// jsonStatusError writes a curated client-facing reason with a specific
// status. Like jsonError it never echoes err.Error() - the reason is one
// this package chose, not one an internal error handed it.
func jsonStatusError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(
		map[string]string{
			"error": reason,
		},
	)
}

// requireGet refuses anything but a GET with "GET required" and reports
// whether the request may go on.
func requireGet(w http.ResponseWriter, r *http.Request) bool {
	return requireMethod(w, r, http.MethodGet)
}

// requirePost refuses anything but a POST with "POST required" and reports
// whether the request may go on.
func requirePost(w http.ResponseWriter, r *http.Request) bool {
	return requireMethod(w, r, http.MethodPost)
}

// requireMethod refuses any method but method with "<method> required" and
// reports whether the request may go on.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, method+" required", http.StatusMethodNotAllowed)
		return false
	}

	return true
}

// allowMethod refuses any method but method with "method not allowed" and
// reports whether the request may go on.
func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}

	return true
}

// requireMethodJSON is requireMethod answering in JSON.
func requireMethodJSON(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		jsonStatusError(w, http.StatusMethodNotAllowed, method+" required")
		return false
	}

	return true
}
