package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/memory"
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
