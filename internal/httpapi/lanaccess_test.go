package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YKoustubhRao/flowera/internal/lanaccess"
)

func TestLANAccessStatusHandler(t *testing.T) {
	s := &Server{LANAccess: lanaccess.NewStore()}

	req := httptest.NewRequest(http.MethodGet, "/api/lan-access/status", nil)
	rec := httptest.NewRecorder()
	s.lanAccessStatusHandler(rec, req)

	var body struct {
		Enabled bool   `json:"enabled"`
		Code    string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if !body.Enabled {
		t.Fatal("expected enabled=true once a Store is configured")
	}

	if body.Code != s.LANAccess.Password() {
		t.Fatalf("code = %q, want %q", body.Code, s.LANAccess.Password())
	}
}

func TestLANAccessStatusHandlerWithNoStoreConfigured(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/lan-access/status", nil)
	rec := httptest.NewRecorder()
	s.lanAccessStatusHandler(rec, req)

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.Enabled {
		t.Fatal("expected enabled=false with no Store configured")
	}
}

func TestLANAccessRotateHandlerChangesTheCode(t *testing.T) {
	s := &Server{LANAccess: lanaccess.NewStore()}
	before := s.LANAccess.Password()

	req := httptest.NewRequest(http.MethodPost, "/api/lan-access/rotate", nil)
	rec := httptest.NewRecorder()
	s.lanAccessRotateHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.Code == before {
		t.Fatal("rotate did not change the code")
	}

	if body.Code != s.LANAccess.Password() {
		t.Fatalf("returned code %q does not match the store's new password %q", body.Code, s.LANAccess.Password())
	}
}

func TestLANAccessRotateHandlerRejectsGET(t *testing.T) {
	s := &Server{LANAccess: lanaccess.NewStore()}

	req := httptest.NewRequest(http.MethodGet, "/api/lan-access/rotate", nil)
	rec := httptest.NewRecorder()
	s.lanAccessRotateHandler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
