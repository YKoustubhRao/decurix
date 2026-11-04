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
	if !strings.Contains(ticketToolbar, "if(refreshing){ return; }") || !strings.Contains(ticketToolbar, "refreshing = false") {
		t.Error("the ticket poll has no in-flight guard")
	}
	if !strings.Contains(resourceAlertPopup, "alerting = true") || !strings.Contains(resourceAlertPopup, "alerting = false") {
		t.Error("the alert poll has no in-flight guard")
	}
}
