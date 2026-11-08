package main

import (
	"strings"
	"testing"
)

func TestPrimaryListenAddrs(t *testing.T) {
	gateways := func() []string { return []string{"172.17.0.1", "172.20.0.1"} }

	t.Setenv("DECURIX_ADMIN_ADDR", "127.0.0.1:9000, 10.0.0.5:9000")
	if got := strings.Join(primaryListenAddrs(gateways), " "); got != "127.0.0.1:9000 10.0.0.5:9000" {
		t.Errorf("override = %q", got)
	}

	t.Setenv("DECURIX_ADMIN_ADDR", "")
	got := primaryListenAddrs(gateways)
	if inContainer() {
		if len(got) != 1 || got[0] != ":8080" {
			t.Errorf("in a container = %v, want [:8080]", got)
		}
		return
	}
	// On the host: loopback first (the required one), then the Docker gateways - never a wildcard.
	want := []string{"127.0.0.1:8080", "172.17.0.1:8080", "172.20.0.1:8080"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("on the host = %v, want %v", got, want)
	}
}

// TestTheGatedListenerCanBeKeptOffTheLAN: a host-run binary can bind the gated listener to one address (W-18).
func TestTheGatedListenerCanBeKeptOffTheLAN(t *testing.T) {
	t.Setenv("DECURIX_LAN_PORT", "")
	t.Setenv("DECURIX_LAN_ADDR", "127.0.0.1:8090")
	if got := lanListenAddr(); got != "127.0.0.1:8090" {
		t.Fatalf("lanListenAddr() = %q", got)
	}
	t.Setenv("DECURIX_LAN_ADDR", "not an address")
	if got := lanListenAddr(); got != defaultLANAddr {
		t.Fatalf("a bad address must fall back to the default, got %q", got)
	}
	t.Setenv("DECURIX_LAN_ADDR", "")
	t.Setenv("DECURIX_LAN_PORT", "9000")
	if got := lanListenAddr(); got != ":9000" {
		t.Fatalf("lanListenAddr() = %q", got)
	}
}

// An eval run's free-memory minimum is the default unless DECURIX_EVAL_MIN_FREE_MB sets one at or above the floor.
func TestEvalMinFreeBytes(t *testing.T) {
	for raw, want := range map[string]uint64{"": 0, "x": 0, "512": 0, "1800": 1800 << 20, " 2048 ": 2048 << 20} {
		t.Setenv("DECURIX_EVAL_MIN_FREE_MB", raw)
		if got := evalMinFreeBytes(); got != want {
			t.Fatalf("DECURIX_EVAL_MIN_FREE_MB=%q: %d, want %d", raw, got, want)
		}
	}
}
