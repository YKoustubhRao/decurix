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
