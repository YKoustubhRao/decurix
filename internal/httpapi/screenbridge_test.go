package httpapi

import "testing"

// The screen's WebSocket, and only it, goes to the bridge; DECURIX_VNC_BRIDGE=0 (or off, false, no) sends it to
// websockify as before (Phase 2 C).
func TestTheScreenStreamGoesToTheBridgeUnlessTurnedOff(t *testing.T) {
	for _, tc := range []struct {
		rest string
		ws   bool
		want bool
	}{
		{"websockify", true, true},
		{"websockify/", true, true},
		{"websockify", false, false},
		{"vnc.html", true, false},
		{"core/rfb.js", false, false},
	} {
		if got := isScreenStream(tc.rest, tc.ws); got != tc.want {
			t.Errorf("isScreenStream(%q, %v) = %v", tc.rest, tc.ws, got)
		}
	}
	for value, want := range map[string]bool{"": true, "1": true, "on": true, "0": false, "off": false, "False": false, "no": false} {
		t.Setenv("DECURIX_VNC_BRIDGE", value)
		if got := screenBridgeOn(); got != want {
			t.Errorf("DECURIX_VNC_BRIDGE=%q: bridge %v, want %v", value, got, want)
		}
	}
}
