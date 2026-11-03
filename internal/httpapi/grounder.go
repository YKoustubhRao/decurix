package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/YKoustubhRao/decurix/internal/grounder"
)

// grounderStatusHandler says whether a grounding model is set up, and which -
// what a desktop's `gui` reads before offering to look for something on the
// screen. Never a key.
func (s *Server) grounderStatusHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.Grounder.Status())
}

// grounderLocateHandler asks the grounding model where something is on a
// screenshot: JSON {"image": <base64 PNG>, "description": "..."} in, {"x",
// "y", "model"} out, in the PNG's own pixels. 503 when none is set up, 422
// when the model gave no position. Primary listener only, beside the hosted
// models' proxy: the desktops reach it over the Docker network.
func (s *Server) grounderLocateHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	if s.Grounder == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, grounder.ErrNotConfigured.Error())
		return
	}

	var req struct {
		Image       string `json:"image"`
		Description string `json:"description"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, grounder.MaxImageBytes*4/3+4096))
	if err != nil || json.Unmarshal(body, &req) != nil {
		jsonStatusError(w, http.StatusBadRequest, "send JSON with image and description")
		return
	}
	image, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil || len(image) > grounder.MaxImageBytes {
		jsonStatusError(w, http.StatusBadRequest, "image must be a base64 PNG")
		return
	}
	config, err := png.DecodeConfig(bytes.NewReader(image))
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "image must be a base64 PNG")
		return
	}
	if grounder.TooLarge(config.Width, config.Height) {
		jsonStatusError(w, http.StatusRequestEntityTooLarge, "the screenshot is larger than a screen")
		return
	}

	start := time.Now()
	point, err := s.Grounder.Locate(r.Context(), image, config.Width, config.Height, req.Description)
	if errors.Is(err, grounder.ErrNoPoint) {
		log.Printf("grounder: no position for %q in %q", req.Description, point.Answer)
		jsonStatusError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		log.Printf("grounder: %v", err)
		jsonStatusError(w, http.StatusBadGateway, "the grounding model could not be reached")
		return
	}
	jsonResponse(w, map[string]any{
		"x": point.X, "y": point.Y, "model": s.Grounder.Model,
		"seconds": time.Since(start).Seconds(),
	})
}
