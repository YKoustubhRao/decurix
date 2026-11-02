package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// personasHandler returns the full catalog of AI employee personas.
func (s *Server) personasHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	jsonResponse(w, map[string]any{
		"personas": desktop.AllPersonas(),
	})
}

// personaHandler returns a single persona by id query param.
func (s *Server) personaHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		jsonStatusError(w, http.StatusBadRequest, "id is required")
		return
	}

	p, ok := desktop.GetPersona(id)
	if !ok {
		jsonStatusError(w, http.StatusNotFound, "persona not found")
		return
	}

	jsonResponse(w, map[string]any{
		"persona": p,
	})
}

// teamsHandler returns all multi-agent team templates.
func (s *Server) teamsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	jsonResponse(w, map[string]any{
		"teams": desktop.AllTeams(),
	})
}

// teamHandler returns a single team template by id query param.
func (s *Server) teamHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		jsonStatusError(w, http.StatusBadRequest, "id is required")
		return
	}

	t, ok := desktop.GetTeam(id)
	if !ok {
		jsonStatusError(w, http.StatusNotFound, "team not found")
		return
	}

	jsonResponse(w, map[string]any{
		"team": t,
	})
}

// personaDesktopSpec is how a persona's desktop is to be made: the fields a
// create and a team deploy request share.
type personaDesktopSpec struct {
	Profile  string `json:"profile"`
	Harness  string `json:"harness"`
	Brain    string `json:"brain"`
	CPUs     int64  `json:"cpus"`
	MemoryMB int64  `json:"memory_mb"`
}

// personaDesktop fills in spec's defaults (basic, 2 CPUs, 4096 MB) and checks
// its brain, answering the request itself (and returning false) when that is
// refused or there is no desktop manager to make the desktop with.
func (s *Server) personaDesktop(w http.ResponseWriter, spec *personaDesktopSpec) (desktop.Brain, bool) {
	spec.Profile = strings.TrimSpace(spec.Profile)
	if spec.Profile == "" {
		spec.Profile = "basic"
	}

	brain, err := s.desktopBrain(spec.Harness, spec.Brain)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return brain, false
	}

	if spec.CPUs <= 0 {
		spec.CPUs = 2
	}

	if spec.MemoryMB <= 0 {
		spec.MemoryMB = 4096
	}

	if s.Desktops == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "desktop manager unavailable")
		return brain, false
	}

	return brain, true
}

type createPersonaDesktopReq struct {
	PersonaID string `json:"persona_id"`
	Name      string `json:"name"`
	personaDesktopSpec
}

// createPersonaDesktopHandler provisions a specialized desktop pre-configured for an AI persona.
func (s *Server) createPersonaDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	var req createPersonaDesktopReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonStatusError(w, http.StatusBadRequest, "invalid json payload")
		return
	}

	p, ok := desktop.GetPersona(req.PersonaID)
	if !ok {
		jsonStatusError(w, http.StatusNotFound, fmt.Sprintf("persona %q not found", req.PersonaID))
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = fmt.Sprintf("%s-worker-%d", p.ID, time.Now().Unix())
	}

	brain, ok := s.personaDesktop(w, &req.personaDesktopSpec)
	if !ok {
		return
	}

	prov, err := s.Desktops.BeginProvisioning(
		name,
		req.Profile,
		desktop.HarnessTheBot,
		brain,
		req.CPUs,
		req.MemoryMB,
		"",
		p.Specialization,
		"",
		nil,
		nil,
		nil,
	)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{
		"status":       "provisioning",
		"persona":      p,
		"provisioning": prov,
	})
}

type deployTeamReq struct {
	TeamID string `json:"team_id"`
	Prefix string `json:"prefix"`
	personaDesktopSpec
}

// deployTeamHandler deploys all personas in a multi-agent team template and links their shared memory.
func (s *Server) deployTeamHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	var req deployTeamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonStatusError(w, http.StatusBadRequest, "invalid json payload")
		return
	}

	team, ok := desktop.GetTeam(req.TeamID)
	if !ok {
		jsonStatusError(w, http.StatusNotFound, fmt.Sprintf("team %q not found", req.TeamID))
		return
	}

	prefix := strings.TrimSpace(req.Prefix)
	if prefix == "" {
		prefix = "team"
	}

	brain, ok := s.personaDesktop(w, &req.personaDesktopSpec)
	if !ok {
		return
	}

	deployed := make([]map[string]any, 0, len(team.PersonaIDs))
	var primaryDesktopID string

	for idx, personaID := range team.PersonaIDs {
		p, found := desktop.GetPersona(personaID)
		if !found {
			continue
		}

		workerName := fmt.Sprintf("%s-%s-%d", prefix, p.ID, time.Now().Unix()%10000+int64(idx))

		parentID := ""
		if idx > 0 && primaryDesktopID != "" {
			parentID = primaryDesktopID
		}

		prov, err := s.Desktops.BeginProvisioning(
			workerName,
			req.Profile,
			desktop.HarnessTheBot,
			brain,
			req.CPUs,
			req.MemoryMB,
			"",
			p.Specialization,
			parentID,
			nil,
			nil,
			nil,
		)
		if err != nil {
			continue
		}

		if idx == 0 {
			primaryDesktopID = prov.ID
		}

		deployed = append(deployed, map[string]any{
			"persona":      p,
			"provisioning": prov,
		})
	}

	jsonResponse(w, map[string]any{
		"status":  "deployed",
		"team":    team,
		"workers": deployed,
		"count":   len(deployed),
	})
}

type inboundChannelReq struct {
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
	Text    string `json:"text"`
	Subject string `json:"subject"`
}

// inboundChannelHandler handles incoming communications from Slack, email, webhooks, and forms
// and routes them to the right departmental employee persona.
func (s *Server) inboundChannelHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	var req inboundChannelReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonStatusError(w, http.StatusBadRequest, "invalid json payload")
		return
	}

	fullText := req.Text
	if req.Subject != "" {
		fullText = req.Subject + ": " + req.Text
	}

	matchedPersona, ok := desktop.MatchPersonaForInbound(req.Channel, req.Sender, fullText)
	if !ok {
		jsonStatusError(w, http.StatusBadRequest, "unable to determine target persona")
		return
	}

	// Find an active running desktop matching this persona's specialization
	var targetDesktop *desktop.Desktop
	if s.Desktops != nil {
		existing, err := s.Desktops.List(r.Context())
		if err == nil {
			for i := range existing {
				d := &existing[i]
				if strings.EqualFold(d.Status, "running") &&
					(strings.Contains(strings.ToLower(d.Specialization), strings.ToLower(matchedPersona.ID)) ||
						strings.Contains(strings.ToLower(d.Name), strings.ToLower(matchedPersona.ID))) {
					targetDesktop = d
					break
				}
			}
			// Fallback to any running desktop if no specialization match
			if targetDesktop == nil {
				for i := range existing {
					d := &existing[i]
					if strings.EqualFold(d.Status, "running") {
						targetDesktop = d
						break
					}
				}
			}
		}
	}

	var taskID, targetID string
	var dispatched bool
	if targetDesktop != nil {
		targetID = targetDesktop.ID
		taskGoal := fmt.Sprintf("[%s via %s from %s] %s", matchedPersona.Name, req.Channel, req.Sender, fullText)
		tID, err := s.SubmitTracked(r.Context(), targetID, taskGoal)
		if err == nil {
			taskID = tID
			dispatched = true
		}
	}

	jsonResponse(w, map[string]any{
		"status":          "received",
		"matched_persona": matchedPersona,
		"channel":         req.Channel,
		"sender":          req.Sender,
		"dispatched":      dispatched,
		"desktop_id":      targetID,
		"task_id":         taskID,
	})
}

// skillsCatalogHandler returns a desktop's skill catalog (with ?id=), or what
// the built-in catalog holds: nothing, since a skill nobody demonstrated here
// is not one a worker knows (agent/decurix_agent/computer/catalog.py).
func (s *Server) skillsCatalogHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	desktopID := strings.TrimSpace(r.URL.Query().Get("id"))
	if desktopID != "" {
		// Proxy to desktop's internal catalog endpoint
		s.callAgent(w, r, desktopID, http.MethodGet, "/catalog", nil, true)
		return
	}

	// Return catalog info
	jsonResponse(w, map[string]any{
		"catalog_count": 0,
		"departments":   []string{},
		"description":   "The built-in catalog is empty: a worker's skills are the ones a person here taught it.",
	})
}

type installCatalogSkillReq struct {
	DesktopID  string `json:"desktop_id"`
	SkillName  string `json:"skill_name"`
	TargetName string `json:"target_name,omitempty"`
}

// installCatalogSkillHandler installs a skill from the standard catalog onto a target desktop.
func (s *Server) installCatalogSkillHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	var req installCatalogSkillReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonStatusError(w, http.StatusBadRequest, "invalid json payload")
		return
	}

	desktopID := strings.TrimSpace(req.DesktopID)
	if desktopID == "" {
		jsonStatusError(w, http.StatusBadRequest, "desktop_id is required")
		return
	}

	skillName := strings.TrimSpace(req.SkillName)
	if skillName == "" {
		jsonStatusError(w, http.StatusBadRequest, "skill_name is required")
		return
	}

	payload := map[string]string{
		"name":        skillName,
		"target_name": req.TargetName,
	}

	s.callAgent(w, r, desktopID, http.MethodPost, "/catalog/install", payload, true)
}

// workerModeHandler retrieves or updates a desktop worker's replication lifecycle stage.
func (s *Server) workerModeHandler(w http.ResponseWriter, r *http.Request) {
	desktopID := strings.TrimSpace(r.URL.Query().Get("id"))
	if desktopID == "" {
		jsonStatusError(w, http.StatusBadRequest, "id is required")
		return
	}

	if r.Method == http.MethodGet {
		s.callAgent(w, r, desktopID, http.MethodGet, "/mode", nil, true)
		return
	}

	if r.Method == http.MethodPost {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonStatusError(w, http.StatusBadRequest, "invalid json payload")
			return
		}
		s.callAgent(w, r, desktopID, http.MethodPost, "/mode", req, true)
		return
	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

