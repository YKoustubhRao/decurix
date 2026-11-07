package httpapi

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
)

// The screen's WebSocket, and only it, goes to the bridge; DECURIX_VNC_BRIDGE=0 (or off, false, no) sends it to a
// desktop's websockify when it has one (Phase 2 C).
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

// With the bridge turned off, a desktop's own websockify is used when it answers; a desktop that runs none (L3) is
// bridged anyway, and one that does not resolve is left to the reverse proxy to report.
func TestScreenStreamFallsBackToTheBridgeWithoutWebsockify(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()
	target := func(addr string, err error) func(context.Context, string) (*url.URL, string, error) {
		return func(context.Context, string) (*url.URL, string, error) {
			if err != nil {
				return nil, "box", err
			}
			return &url.URL{Scheme: "http", Host: addr}, "box", nil
		}
	}
	ctx := context.Background()
	if websockifyMissing(ctx, target(ln.Addr().String(), nil), "box") {
		t.Error("a desktop whose websockify answers was bridged")
	}
	if !websockifyMissing(ctx, target(closedAddr, nil), "box") {
		t.Error("a desktop with no websockify was not bridged")
	}
	if websockifyMissing(ctx, target("", errors.New("desktop is not running")), "box") {
		t.Error("a desktop that does not resolve was bridged instead of reported")
	}
}
