package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPersonasAPI(t *testing.T) {
	s := chatServer(t)

	// List personas
	req := httptest.NewRequest(http.MethodGet, "/api/personas", nil)
	rec := httptest.NewRecorder()
	s.personasHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var listResp struct {
		Personas []map[string]any `json:"personas"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("failed to parse json: %v", err)
	}
	if len(listResp.Personas) < 8 {
		t.Errorf("expected at least 8 personas, got %d", len(listResp.Personas))
	}

	// Get single persona
	reqSingle := httptest.NewRequest(http.MethodGet, "/api/personas/get?id=finance", nil)
	recSingle := httptest.NewRecorder()
	s.personaHandler(recSingle, reqSingle)

	if recSingle.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recSingle.Code)
	}

	var singleResp struct {
		Persona map[string]any `json:"persona"`
	}
	if err := json.NewDecoder(recSingle.Body).Decode(&singleResp); err != nil {
		t.Fatalf("failed to parse json: %v", err)
	}
	if singleResp.Persona["id"] != "finance" {
		t.Errorf("expected finance, got %v", singleResp.Persona["id"])
	}

	// Not found
	reqNotFound := httptest.NewRequest(http.MethodGet, "/api/personas/get?id=unknown", nil)
	recNotFound := httptest.NewRecorder()
	s.personaHandler(recNotFound, reqNotFound)
	if recNotFound.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", recNotFound.Code)
	}
}

func TestTeamsAPI(t *testing.T) {
	s := chatServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	rec := httptest.NewRecorder()
	s.teamsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var teamsResp struct {
		Teams []map[string]any `json:"teams"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&teamsResp); err != nil {
		t.Fatalf("failed to parse json: %v", err)
	}
	if len(teamsResp.Teams) < 3 {
		t.Errorf("expected at least 3 teams, got %d", len(teamsResp.Teams))
	}

	// Get single team
	reqSingle := httptest.NewRequest(http.MethodGet, "/api/teams/get?id=business-core", nil)
	recSingle := httptest.NewRecorder()
	s.teamHandler(recSingle, reqSingle)

	if recSingle.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recSingle.Code)
	}
}

func TestSkillsCatalogAPI(t *testing.T) {
	s := chatServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/skills/catalog", nil)
	rec := httptest.NewRecorder()
	s.skillsCatalogHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var catalogResp struct {
		CatalogCount int      `json:"catalog_count"`
		Departments  []string `json:"departments"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&catalogResp); err != nil {
		t.Fatalf("failed to decode catalog response: %v", err)
	}
	if catalogResp.CatalogCount != 22 {
		t.Errorf("expected 22 catalog skills, got %d", catalogResp.CatalogCount)
	}
}

func TestInboundChannelRouting(t *testing.T) {
	s := chatServer(t)

	body := bytes.NewBufferString(`{
		"channel": "email",
		"sender": "vendor@billing.com",
		"text": "Invoice #1024 attached for payment and reconciliation",
		"subject": "Vendor Invoice"
	}`)

	req := httptest.NewRequest(http.MethodPost, "/api/channels/inbound", body)
	rec := httptest.NewRecorder()
	s.inboundChannelHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status         string         `json:"status"`
		MatchedPersona map[string]any `json:"matched_persona"`
		Channel        string         `json:"channel"`
		Sender         string         `json:"sender"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to parse json: %v", err)
	}

	if resp.Status != "received" {
		t.Errorf("expected status received, got %s", resp.Status)
	}
	if resp.MatchedPersona["id"] != "finance" {
		t.Errorf("expected persona finance, got %v", resp.MatchedPersona["id"])
	}
}
