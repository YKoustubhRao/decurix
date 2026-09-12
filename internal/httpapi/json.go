package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
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

	jsonStatusError(
		w,
		http.StatusInternalServerError,
		"internal server error",
	)
}

// jsonStatusError writes a curated client-facing message with a specific
// status. Like jsonError it never echoes err.Error() - the message is one
// this package chose, not one an internal error handed it.
func jsonStatusError(w http.ResponseWriter, status int, message string) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(
		map[string]string{
			"error": message,
		},
	)
}
