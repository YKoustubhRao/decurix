package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/cftunnel"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tunnel"
)

func TestTunnelHandlerPostThenGet(t *testing.T) {
	s := &Server{Tunnel: tunnel.NewStore()}

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/tunnel", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.tunnelHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s: status = %d", body, rec.Code)
		}
	}

	// The admin/LAN URLs and the two tunnel URLs are reported by separate
	// calls - none should blank out another.
	post(`{"admin_url":"http://192.168.1.42:8080","lan_url":"http://192.168.1.42:8090"}`)
	post(`{"url":"https://foo.trycloudflare.com"}`)
	post(`{"lan_tunnel_url":"https://bar.trycloudflare.com"}`)
	post(`{"lan_tunnel_url":""}`) // stopping the app clears its temporary link
	if got := s.Tunnel.LANTunnelURL(); got != "" {
		t.Fatalf("temporary tunnel was not cleared: %q", got)
	}
	post(`{"lan_tunnel_url":"https://bar.trycloudflare.com"}`)

	req := httptest.NewRequest(http.MethodGet, "/api/tunnel", nil)
	rec := httptest.NewRecorder()
	s.tunnelHandler(rec, req)

	var body struct {
		URL          string `json:"url"`
		AdminURL     string `json:"admin_url"`
		LANURL       string `json:"lan_url"`
		LANTunnelURL string `json:"lan_tunnel_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.URL != "https://foo.trycloudflare.com" {
		t.Errorf("url = %q", body.URL)
	}
	if body.AdminURL != "http://192.168.1.42:8080" {
		t.Errorf("admin_url = %q", body.AdminURL)
	}
	if body.LANURL != "http://192.168.1.42:8090" {
		t.Errorf("lan_url = %q", body.LANURL)
	}
	if body.LANTunnelURL != "https://bar.trycloudflare.com" {
		t.Errorf("lan_tunnel_url = %q", body.LANTunnelURL)
	}
}

func TestAFixedLinkIsReportedOverAQuickTunnel(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	named, err := cftunnel.NewManager(db, nil, 8090)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{Tunnel: tunnel.NewStore(), CFTunnel: named}
	s.Tunnel.SetLANTunnelURL("https://bar.trycloudflare.com")

	get := func() string {
		rec := httptest.NewRecorder()
		s.tunnelHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tunnel", nil))
		var body struct {
			LANTunnelURL string `json:"lan_tunnel_url"`
		}
		json.NewDecoder(rec.Body).Decode(&body)
		return body.LANTunnelURL
	}

	if got := get(); got != "https://bar.trycloudflare.com" {
		t.Fatalf("with no fixed link: %q", got)
	}

	// A setup saved by an earlier run, as NewManager finds it on startup.
	if _, err := db.Exec(`INSERT INTO cftunnel_config
		(id, api_token, domain, hostname, account_id, zone_id, tunnel_id, tunnel_name, connector_token, updated_ns)
		VALUES (1, 'tok', 'example.com', 'decurix.example.com', 'a', 'z', 't', 'n', 'c', 0)`); err != nil {
		t.Fatal(err)
	}
	named, err = cftunnel.NewManager(db, nil, 8090)
	if err != nil {
		t.Fatal(err)
	}
	s.CFTunnel = named

	if got := get(); got != "https://decurix.example.com" {
		t.Fatalf("with a fixed link: %q, want it over the quick tunnel", got)
	}
}

func TestCloudflaresAPIHostIsNeverTakenForTheTunnel(t *testing.T) {
	s := &Server{Tunnel: tunnel.NewStore()}

	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/api/tunnel", strings.NewReader(body))
		s.tunnelHandler(httptest.NewRecorder(), req)
	}
	get := func() string {
		rec := httptest.NewRecorder()
		s.tunnelHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tunnel", nil))
		var body struct {
			LANTunnelURL string `json:"lan_tunnel_url"`
		}
		json.NewDecoder(rec.Body).Decode(&body)
		return body.LANTunnelURL
	}

	post(`{"lan_tunnel_url":"https://api.trycloudflare.com"}`)
	if got := get(); got != "" {
		t.Fatalf("the API host was taken as the tunnel: %q", got)
	}

	post(`{"lan_tunnel_url":"https://seasonal-deck-organisms-sf.trycloudflare.com"}`)
	if got := get(); got != "https://seasonal-deck-organisms-sf.trycloudflare.com" {
		t.Fatalf("a real tunnel address was dropped: %q", got)
	}

	// Stored before the check existed: not shown either.
	s.Tunnel.SetLANTunnelURL("https://api.trycloudflare.com")
	if got := get(); got != "" {
		t.Fatalf("a stored API host is still shown: %q", got)
	}
}
