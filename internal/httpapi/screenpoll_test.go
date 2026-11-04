package httpapi

import (
	"strings"
	"testing"
)

// The screen tab's own polls (the recording ticket and the resource alert, injected into the noVNC page) ask one at
// a time, not while the tab is hidden, and at once when it is seen again (resource plan P1 #9).
func TestTheScreenTabsPollsRespectVisibilityAndWaitForTheirCall(t *testing.T) {
	for name, script := range map[string]string{"ticketToolbar": ticketToolbar, "resourceAlertPopup": resourceAlertPopup} {
		for _, want := range []string{`document.visibilityState`, `addEventListener("visibilitychange"`} {
			if !strings.Contains(script, want) {
				t.Errorf("%s lacks %s", name, want)
			}
		}
	}
	// One at a time, but a call in flight past max(30 s, 4 polls) is given up on and its late answer ignored, so one
	// answer that never comes cannot stop the poll (review RES-CP-1).
	for _, want := range []string{"Date.now() - refreshSince < Math.max(30000, 4 * POLL_MS)", "if(busy || call !== refreshing){ return; }",
		"if(call === refreshing){ refreshing = 0; }"} {
		if !strings.Contains(ticketToolbar, want) {
			t.Errorf("the ticket poll lacks %q", want)
		}
	}
	for _, want := range []string{"Date.now() - alertSince < Math.max(30000, 4 * 4000)", "if (call !== alerting) return;",
		"if (call === alerting) alerting = 0;"} {
		if !strings.Contains(resourceAlertPopup, want) {
			t.Errorf("the alert poll lacks %q", want)
		}
	}
}
