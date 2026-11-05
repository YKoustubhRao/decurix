package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/docker/docker/client"
)

// resourceAlertsHandler returns active resource alerts.
// GET /api/desktops/resource-alerts?id=<desktop_id>
func (s *Server) resourceAlertsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	if s.ResourceMon == nil {
		jsonResponse(w, map[string]any{"alert": nil, "alerts": []any{}})
		return
	}

	desktopID := r.URL.Query().Get("id")
	if desktopID == "" {
		desktopID = r.URL.Query().Get("desktop_id")
	}

	if desktopID != "" {
		alert := s.ResourceMon.GetAlert(desktopID)
		jsonResponse(w, map[string]any{
			"alert": alert,
		})
		return
	}

	alerts := s.ResourceMon.ListAlerts()
	jsonResponse(w, map[string]any{
		"alerts": alerts,
	})
}

// resourceDesktopID is the desktop a resource call names: desktop_id, else id,
// from the form or the query string.
func resourceDesktopID(r *http.Request) string {
	if id := r.FormValue("desktop_id"); id != "" {
		return id
	}
	return r.FormValue("id")
}

// resourceIncreaseHandler applies the predicted (or requested) resource increase when the user clicks Yes.
// POST /api/desktops/resource-increase
func (s *Server) resourceIncreaseHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	desktopID := resourceDesktopID(r)

	var reqCPUs, reqMemMB int64

	// Also parse form or JSON body
	if r.Header.Get("Content-Type") == "application/json" {
		var req struct {
			DesktopID string `json:"desktop_id"`
			CPUs      int64  `json:"cpus"`
			MemoryMB  int64  `json:"memory_mb"`
		}
		limitJSON(w, r)
		// A body that does not decode (cut at its cap, or malformed) is refused, never read as "the predicted
		// increase" (RW1-E-05).
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badJSON(w, err)
			return
		}
		if desktopID == "" {
			desktopID = req.DesktopID
		}
		reqCPUs = req.CPUs
		reqMemMB = req.MemoryMB
	} else {
		reqCPUs = positiveInt(r.FormValue("cpus"), 0)
		reqMemMB = positiveInt(r.FormValue("memory_mb"), 0)
	}

	if desktopID == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}

	if s.ResourceMon == nil {
		http.Error(w, "resource monitor not enabled", http.StatusServiceUnavailable)
		return
	}

	res, err := s.ResourceMon.Increase(r.Context(), desktopID, reqCPUs, reqMemMB)
	if err != nil {
		// Never echo the Docker daemon's own words back to a client.
		if client.IsErrNotFound(err) {
			err = fmt.Errorf("increase resources for %s: %w", desktopID, desktop.ErrNotFound)
		}
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{
		"status":     "ok",
		"desktop_id": desktopID,
		"cpus":       res.PredictedCPUs,
		"memory_mb":  res.PredictedMemoryMB,
		"message":    "Resources successfully increased",
	})
}

// resourceDismissHandler dismisses an alert when the user clicks No.
// POST /api/desktops/resource-dismiss
func (s *Server) resourceDismissHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	desktopID := resourceDesktopID(r)

	if r.Header.Get("Content-Type") == "application/json" {
		var req struct {
			DesktopID string `json:"desktop_id"`
		}
		limitJSON(w, r)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badJSON(w, err)
			return
		}
		if req.DesktopID != "" {
			desktopID = req.DesktopID
		}
	}

	if desktopID == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}

	if s.ResourceMon != nil {
		s.ResourceMon.Dismiss(desktopID)
	}

	jsonResponse(w, map[string]any{
		"status":     "dismissed",
		"desktop_id": desktopID,
	})
}

// resourceReportHandler allows reporting CPU/RAM usage to trigger an alert.
// POST /api/desktops/resource-report
func (s *Server) resourceReportHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		DesktopID     string  `json:"desktop_id"`
		CPUPercent    float64 `json:"cpu_percent"`
		MemoryPercent float64 `json:"memory_percent"`
	}

	limitJSON(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badJSON(w, err)
		return
	}

	if req.DesktopID == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}

	if s.ResourceMon == nil {
		http.Error(w, "resource monitor not enabled", http.StatusServiceUnavailable)
		return
	}

	alert, err := s.ResourceMon.RecordReport(req.DesktopID, req.CPUPercent, req.MemoryPercent)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{
		"status": "ok",
		"alert":  alert,
	})
}
