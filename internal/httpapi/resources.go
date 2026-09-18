package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// resourceAlertsHandler returns active resource alerts.
// GET /api/desktops/resource-alerts?id=<desktop_id>
func (s *Server) resourceAlertsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if s.ResourceMon == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"alert": nil, "alerts": []any{}})
		return
	}

	desktopID := r.URL.Query().Get("id")
	if desktopID == "" {
		desktopID = r.URL.Query().Get("desktop_id")
	}

	if desktopID != "" {
		alert := s.ResourceMon.GetAlert(desktopID)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"alert": alert,
		})
		return
	}

	alerts := s.ResourceMon.ListAlerts()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"alerts": alerts,
	})
}

// resourceIncreaseHandler applies the predicted (or requested) resource increase when the user clicks Yes.
// POST /api/desktops/resource-increase
func (s *Server) resourceIncreaseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	desktopID := r.URL.Query().Get("desktop_id")
	if desktopID == "" {
		desktopID = r.URL.Query().Get("id")
	}

	var reqCPUs, reqMemMB int64

	// Also parse form or JSON body
	if r.Header.Get("Content-Type") == "application/json" {
		var req struct {
			DesktopID string `json:"desktop_id"`
			CPUs      int64  `json:"cpus"`
			MemoryMB  int64  `json:"memory_mb"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			if desktopID == "" {
				desktopID = req.DesktopID
			}
			reqCPUs = req.CPUs
			reqMemMB = req.MemoryMB
		}
	} else {
		_ = r.ParseForm()
		if id := r.FormValue("desktop_id"); id != "" {
			desktopID = id
		} else if id := r.FormValue("id"); id != "" {
			desktopID = id
		}
		if c, err := strconv.ParseInt(r.FormValue("cpus"), 10, 64); err == nil && c > 0 {
			reqCPUs = c
		}
		if m, err := strconv.ParseInt(r.FormValue("memory_mb"), 10, 64); err == nil && m > 0 {
			reqMemMB = m
		}
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	desktopID := r.URL.Query().Get("desktop_id")
	if desktopID == "" {
		desktopID = r.URL.Query().Get("id")
	}

	if r.Header.Get("Content-Type") == "application/json" {
		var req struct {
			DesktopID string `json:"desktop_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.DesktopID != "" {
			desktopID = req.DesktopID
		}
	} else {
		_ = r.ParseForm()
		if id := r.FormValue("desktop_id"); id != "" {
			desktopID = id
		} else if id := r.FormValue("id"); id != "" {
			desktopID = id
		}
	}

	if desktopID == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}

	if s.ResourceMon != nil {
		s.ResourceMon.Dismiss(desktopID)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "dismissed",
		"desktop_id": desktopID,
	})
}

// resourceReportHandler allows reporting CPU/RAM usage to trigger an alert.
// POST /api/desktops/resource-report
func (s *Server) resourceReportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DesktopID     string  `json:"desktop_id"`
		CPUPercent    float64 `json:"cpu_percent"`
		MemoryPercent float64 `json:"memory_percent"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"alert":  alert,
	})
}
