package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
