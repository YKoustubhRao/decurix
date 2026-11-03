package desktop

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNetViewClassify(t *testing.T) {
	v := NetView{
		Subnets:  []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("172.25.0.0/16")},
		Gateways: []netip.Addr{netip.MustParseAddr("172.18.0.1")},
	}
	for addr, want := range map[string]Origin{
		"127.0.0.1":         OriginHost,
		"::1":               OriginHost,
		"172.18.0.1":        OriginHost,
		"::ffff:172.18.0.1": OriginHost,
		"172.18.0.5":        OriginContainer,
		"172.25.0.1":        OriginContainer, // another network's gateway is not the host's way in
		"172.25.0.9":        OriginContainer,
		"192.168.1.20":      OriginUnknown,
	} {
		if got := v.Classify(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Classify(%s) = %v, want %v", addr, got, want)
		}
	}
	if (NetView{}).Classify(netip.Addr{}) != OriginUnknown {
		t.Error("an unreadable address was classified")
	}
}

// The control plane in a compose container knows its own network and the gateway a published port arrives from
// without asking Docker; on a host, the Docker bridges are container ranges; with host networking a container's
// interfaces are the host's, and its LAN range must never be taken for a container network.
func TestInterfaceNetView(t *testing.T) {
	ifaces := func(names ...string) []net.Interface {
		var out []net.Interface
		for _, n := range names {
			flags := net.FlagUp
			if n == "lo" {
				flags |= net.FlagLoopback
			}
			out = append(out, net.Interface{Name: n, Flags: flags})
		}
		return out
	}
	prefixes := map[string][]netip.Prefix{
		"lo":      {netip.MustParsePrefix("127.0.0.0/8")},
		"eth0":    {netip.MustParsePrefix("172.18.0.0/16")},
		"wlan0":   {netip.MustParsePrefix("192.168.1.0/24")},
		"docker0": {netip.MustParsePrefix("172.17.0.0/16")},
		"br-1a2b": {netip.MustParsePrefix("172.25.0.0/16")},
	}
	of := func(i net.Interface) []netip.Prefix { return prefixes[i.Name] }
	gws := func(list ...string) func() []netip.Addr {
		return func() []netip.Addr {
			var out []netip.Addr
			for _, a := range list {
				out = append(out, netip.MustParseAddr(a))
			}
			return out
		}
	}

	compose := interfaceNetView(ifaces("lo", "eth0"), of, true, gws("172.18.0.1"))
	if compose.Classify(netip.MustParseAddr("172.18.0.1")) != OriginHost || compose.Classify(netip.MustParseAddr("172.18.0.7")) != OriginContainer {
		t.Errorf("compose: %+v", compose)
	}

	host := interfaceNetView(ifaces("lo", "wlan0", "docker0", "br-1a2b"), of, false, gws("192.168.1.1"))
	if host.Classify(netip.MustParseAddr("172.25.0.3")) != OriginContainer || host.Classify(netip.MustParseAddr("192.168.1.20")) != OriginUnknown {
		t.Errorf("host: %+v", host)
	}
	if len(host.Gateways) != 0 {
		t.Errorf("a host's own interfaces gave gateways: %v", host.Gateways)
	}

	hostNet := interfaceNetView(ifaces("lo", "wlan0", "docker0"), of, true, gws("192.168.1.1"))
	if hostNet.Classify(netip.MustParseAddr("192.168.1.20")) != OriginUnknown || hostNet.Classify(netip.MustParseAddr("192.168.1.1")) != OriginUnknown {
		t.Errorf("host networking took the LAN for a Docker network: %+v", hostNet)
	}
}

func TestParseDefaultGateways(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\n" +
		"eth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\n"
	got := parseDefaultGateways(strings.NewReader(table))
	if len(got) != 1 || got[0] != netip.MustParseAddr("172.18.0.1") {
		t.Fatalf("default gateways = %v", got)
	}
}

// Found in review (2026-11-02): every lookup asked Docker under one lock, so phone and LAN polling queued behind it.
// A known address in a fresh map is answered without waiting, a burst of unknown ones makes one Docker call, and an
// address that is no desktop's asks again at most every callerRefreshEvery.
func TestDesktopAtSharesOneDockerCall(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	m := &Manager{}
	m.callers.list = func(context.Context) (map[netip.Addr]string, error) {
		if calls.Add(1) > 1 {
			<-release
		}
		return map[netip.Addr]string{netip.MustParseAddr("172.18.0.5"): "box-a"}, nil
	}
	if id, ok := m.DesktopAt(context.Background(), netip.MustParseAddr("172.18.0.5")); !ok || id != "box-a" {
		t.Fatalf("first lookup: %q %v", id, ok)
	}

	// An unknown address past callerRefreshEvery asks Docker again, and that call hangs.
	m.callers.byAddr.mu.Lock()
	m.callers.byAddr.at = time.Now().Add(-callerRefreshEvery - time.Second)
	m.callers.byAddr.mu.Unlock()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			m.DesktopAt(ctx, netip.MustParseAddr("172.18.0.9"))
		}()
	}
	for deadline := time.Now().Add(time.Second); calls.Load() < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	// Meanwhile a known desktop is answered at once.
	start := time.Now()
	if id, ok := m.DesktopAt(context.Background(), netip.MustParseAddr("172.18.0.5")); !ok || id != "box-a" {
		t.Fatalf("known desktop during a refresh: %q %v", id, ok)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("a known desktop waited %s for Docker", waited)
	}
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 2 {
		t.Fatalf("Docker was asked %d times, want 2 (one shared by the burst)", n)
	}
	// Asked again just now: an address that is no desktop's does not ask again yet.
	if _, ok := m.DesktopAt(context.Background(), netip.MustParseAddr("172.18.0.9")); ok || calls.Load() != 2 {
		t.Fatalf("an unknown address asked Docker again within callerRefreshEvery (%d calls)", calls.Load())
	}
}

// A desktop made or removed here drops the map at once: an address handed to a new desktop is never read as the old
// one's for the rest of callerTTL.
func TestDesktopAtFollowsTheDesktopList(t *testing.T) {
	owner := "box-a"
	m := &Manager{}
	m.callers.list = func(context.Context) (map[netip.Addr]string, error) {
		return map[netip.Addr]string{netip.MustParseAddr("172.18.0.5"): owner}, nil
	}
	if id, _ := m.DesktopAt(context.Background(), netip.MustParseAddr("172.18.0.5")); id != "box-a" {
		t.Fatalf("got %q", id)
	}
	owner = "box-b"
	m.invalidateList()
	if id, _ := m.DesktopAt(context.Background(), netip.MustParseAddr("172.18.0.5")); id != "box-b" {
		t.Fatalf("after a desktop change the old owner was kept: %q", id)
	}
}

// What Docker said of its networks is kept through a hiccup, the first answer is waited for, and later answers come
// from the view held while Docker is asked again.
func TestNetworksKeepsWhatDockerSaid(t *testing.T) {
	old := localNetView
	localNetView = func() NetView { return NetView{} }
	t.Cleanup(func() { localNetView = old })

	var fail atomic.Bool
	var calls atomic.Int32
	m := &Manager{}
	m.callers.netsFetch = func(context.Context) (NetView, bool) {
		calls.Add(1)
		if fail.Load() {
			return NetView{}, false
		}
		return NetView{Subnets: []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.18.0.1")}}, true
	}
	if v := m.Networks(context.Background()); v.Classify(netip.MustParseAddr("172.18.0.5")) != OriginContainer {
		t.Fatalf("first view: %+v", v)
	}
	fail.Store(true)
	m.callers.nets.mu.Lock()
	m.callers.nets.at = time.Now().Add(-netsTTL)
	m.callers.nets.mu.Unlock()
	m.Networks(context.Background())
	for deadline := time.Now().Add(time.Second); calls.Load() < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if v := m.Networks(context.Background()); v.Classify(netip.MustParseAddr("172.18.0.5")) != OriginContainer {
		t.Fatalf("a Docker hiccup forgot the ranges: %+v", v)
	}
}
