package httpapi

import (
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
	// The built-in catalog is empty (agent/decurix_agent/computer/catalog.py):
	// the count says so rather than advertise skills nobody taught.
	if catalogResp.CatalogCount != 0 || len(catalogResp.Departments) != 0 {
		t.Errorf("expected an empty catalog, got %d skills in %v", catalogResp.CatalogCount, catalogResp.Departments)
	}
}
