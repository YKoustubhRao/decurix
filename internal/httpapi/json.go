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
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	w.WriteHeader(http.StatusInternalServerError)

	_ = json.NewEncoder(w).Encode(
		map[string]string{
			"error": "internal server error",
		},
	)
}
