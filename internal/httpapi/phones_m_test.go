package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Revoking a paired phone in the Mobile menu ends its live link at once, not
// only its next request (review 2026-11-03, B-18).
func TestRevokingAPhoneClosesItsLiveLink(t *testing.T) {
	s, srv, token, dev := phoneServer(t)
	linkFakePhone(t, srv, token)
	waitFor(t, 3*time.Second, "the phone to link", func() bool { return len(s.Phones.Phones()) > 0 })

	req := httptest.NewRequest(http.MethodPost, "/api/lan-access/devices/revoke", strings.NewReader(url.Values{"id": {dev.ID}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.lanAccessRevokeDeviceHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}

	if n := len(s.Phones.Phones()); n != 0 {
		t.Fatalf("%d phones still linked after their device was revoked", n)
	}
}
