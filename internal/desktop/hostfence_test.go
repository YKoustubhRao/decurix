package desktop

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// onWSLHost makes the test the Windows app's WSL host, whose dockerd.sh last said status ("" for nothing said).
func onWSLHost(t *testing.T, status string) {
	t.Helper()
	t.Setenv("DECURIX_HOST_KIND", "wsl")
	path := filepath.Join(t.TempDir(), "fence")
	if status != "" {
		if err := os.WriteFile(path, []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saved := fenceStatusPath
	fenceStatusPath = path
	t.Cleanup(func() { fenceStatusPath = saved })
}

// On the Windows app's WSL host, a desktop is neither made nor started unless dockerd.sh says the fence holds - a
// fence that failed, was taken out, or was never said is refused in plain words; elsewhere nothing is asked
// (review NET-R3-2).
func TestNoDesktopRunsOnTheWSLHostWithoutItsFence(t *testing.T) {
	t.Setenv("DECURIX_HOST_KIND", "")
	if st := HostFence(); st.Required || requireFence() != nil {
		t.Fatalf("a host that needs no fence: %+v", st)
	}
	for _, status := range []string{"", "state=failed\npool=10.213.0.0/16\n", "state=off\npool=10.213.0.0/16\n", "state=ok\n"} {
		onWSLHost(t, status)
		if st := HostFence(); !st.Required || st.Holds {
			t.Errorf("status %q read as %+v", status, st)
		}
		if err := requireFence(); !errors.Is(err, ErrNoFence) {
			t.Errorf("status %q: %v", status, err)
		}
		if _, err := newManagerWith(&fakeAPI{}).BeginProvisioning("box", "basic", "thebot", Brain{}, 1, 1024, "", "", "", nil, nil, nil); !errors.Is(err, ErrNoFence) {
			t.Errorf("status %q: a desktop was made: %v", status, err)
		}
		fake := &fakeAPI{}
		if err := newManagerWith(fake).Start(context.Background(), "box"); !errors.Is(err, ErrNoFence) || len(fake.called("ContainerStart")) != 0 {
			t.Errorf("status %q: a desktop was started: %v", status, err)
		}
	}
	onWSLHost(t, "state=ok\npool=10.213.0.0/16\nours=10.212.0.0/15\n")
	if st := HostFence(); !st.Holds || st.Pool != "10.213.0.0/16" || requireFence() != nil {
		t.Fatalf("a fence that holds: %+v", HostFence())
	}
}

// On the WSL host the desktops' range must be inside what the fence covers, the one source of truth dockerd.sh
// writes: a DECURIX_DESKTOP_SUBNETS outside it would get no fence, no guard and no NAT, and is refused.
func TestTheDesktopRangeMustBeInsideTheFence(t *testing.T) {
	onWSLHost(t, "state=ok\npool=10.213.0.0/16\nours=10.212.0.0/15\n")
	for value, ok := range map[string]bool{"": true, "10.213.0.0/16": true, "10.213.64.0/18": true, "10.214.0.0/16": false, "10.0.0.0/8": false, "192.168.50.0/24": false} {
		t.Setenv("DECURIX_DESKTOP_SUBNETS", value)
		_, err := fencedSubnetRange()
		if ok != (err == nil) || (!ok && !errors.Is(err, ErrSubnetsOutsideFence)) {
			t.Errorf("DECURIX_DESKTOP_SUBNETS=%q: %v", value, err)
		}
	}
	t.Setenv("DECURIX_DESKTOP_SUBNETS", "10.214.0.0/16")
	fake := &fakeAPI{}
	if _, err := newManagerWith(fake).ensureDesktopNetwork(context.Background(), "box"); !errors.Is(err, ErrSubnetsOutsideFence) || len(fake.called("NetworkCreate")) != 0 {
		t.Fatalf("a network outside the fence: %v, creates %v", err, fake.called("NetworkCreate"))
	}
}

// The host's own routes (a VPN, a LAN) are read from its route table, Docker's bridges and the default route left
// out, and no desktop network is put on a range one of them covers (review NET-R3-2).
func TestADesktopNetworkSkipsTheHostsOwnRoutes(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" + // default via 192.168.1.1
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" + // 192.168.1.0/24
		"tun0\t0000D50A\t00000000\t0001\t0\t0\t0\t00FCFFFF\t0\t0\t0\n" + // 10.213.0.0/22, a VPN
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" + // 172.17.0.0/16
		"br-1a2b\t0001D50A\t00000000\t0001\t0\t0\t0\tF0FFFFFF\t0\t0\t0\n" // 10.213.1.0/28, a desktop network
	got := parseHostRoutes(strings.NewReader(table))
	want := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("10.213.0.0/22")}
	if !slices.Equal(got, want) {
		t.Fatalf("routes %v, want %v", got, want)
	}

	t.Setenv("DECURIX_HOST_KIND", "")
	t.Setenv("DECURIX_DESKTOP_SUBNETS", "")
	saved := readHostRouteTable
	readHostRouteTable = func(context.Context) (io.Reader, error) { return strings.NewReader(table), nil }
	hostRouteCache.at = hostRouteCache.at.AddDate(-1, 0, 0)
	t.Cleanup(func() { readHostRouteTable = saved; hostRouteCache.at = hostRouteCache.at.AddDate(-1, 0, 0) })
	fake := &fakeAPI{}
	fake.putNetwork(network.Summary{Name: NetworkName, IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.213.4.0/24"}}}})
	fake.inspect = func(string) (container.InspectResponse, error) { return container.InspectResponse{}, errFakeNotFound }
	if _, err := newManagerWith(fake).ensureDesktopNetwork(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if got := fake.called("NetworkSubnet"); !slices.Equal(got, []string{"NetworkSubnet decurix-vm-box 10.213.5.0/28"}) {
		t.Fatalf("subnet %v: want the first /28 clear of the VPN's 10.213.0.0/22 and decurix-network's 10.213.4.0/24", got)
	}
}
