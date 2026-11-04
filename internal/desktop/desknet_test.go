package desktop

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/YKoustubhRao/decurix/internal/ollama"
)

// A desktop's container is on its own network only, and another desktop's network is another (resource plan R3:
// desktops share no network, so none reaches another's 5901, 6901 or 8081).
func TestADesktopIsOnItsOwnNetworkOnly(t *testing.T) {
	_, _, box := buildContainerConfigs("img", "desktop_box", "basic", "thebot", 1, 1024, nil, "vol", nil)
	_, _, other := buildContainerConfigs("img", "desktop_other", "basic", "thebot", 1, 1024, nil, "vol", nil)
	keys := func(n *network.NetworkingConfig) []string {
		var out []string
		for k := range n.EndpointsConfig {
			out = append(out, k)
		}
		return out
	}
	if got := keys(box); !slices.Equal(got, []string{DesktopNetworkName("box")}) {
		t.Fatalf("desktop box is on %v, want only %s", got, DesktopNetworkName("box"))
	}
	if DesktopNetworkName("box") == DesktopNetworkName("other") || slices.Equal(keys(box), keys(other)) {
		t.Fatal("two desktops share a network")
	}
	if slices.Contains(keys(box), NetworkName) {
		t.Fatal("a new desktop is on the shared network")
	}
}

// asControlPlane makes the test the control plane's container "cp" (cid-cp), as compose runs it.
func asControlPlane(t *testing.T, fake *fakeAPI) {
	t.Helper()
	savedIn, savedHost := joinsAsContainer, controlPlaneHost
	joinsAsContainer = func() bool { return true }
	controlPlaneHost = func() (string, error) { return "cp", nil }
	t.Cleanup(func() { joinsAsContainer, controlPlaneHost = savedIn, savedHost })
	inner := fake.inspect
	fake.inspect = func(name string) (container.InspectResponse, error) {
		if name == "cp" {
			return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-cp"}, NetworkSettings: &container.NetworkSettings{}}, nil
		}
		if inner != nil {
			return inner(name)
		}
		return container.InspectResponse{}, errFakeNotFound
	}
}

// Desktop networks sort after decurix-network and decurix-practice, and the control plane joins each with the lowest
// gateway priority: Docker gives the default route and the published ports to the highest priority and, on a tie, the
// name that sorts first, and a desktop network that won them took the person's way in (review NET-R3 B2).
func TestAControlPlaneJoinNeverTakesItsGatewayOrPorts(t *testing.T) {
	for _, other := range []string{NetworkName, PracticeNetworkName} {
		if name := DesktopNetworkName("0"); name <= other {
			t.Errorf("%s sorts before %s", name, other)
		}
	}
	fake := &fakeAPI{}
	asControlPlane(t, fake)
	m := newManagerWith(fake)
	if _, err := m.ensureDesktopNetwork(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if p, ok := fake.joinedWith(DesktopNetworkName("box"), "cid-cp"); !ok || p >= 0 {
		t.Fatalf("the control plane joined with gateway priority %d (joined %v), want below the default 0", p, ok)
	}
}

// Making a desktop's network labels it with the desktop and joins the control plane, and nothing else: no model
// server is on a desktop network (review NET-R3 B3). A second call makes nothing; JoinDesktopNetworks (a control
// plane made again) joins every desktop network, and only those whose name and label agree (S4).
func TestADesktopNetworkIsMadeOnceAndJoined(t *testing.T) {
	fake := &fakeAPI{}
	asControlPlane(t, fake)
	m := newManagerWith(fake)
	ctx := context.Background()
	made, err := m.ensureDesktopNetwork(ctx, "box")
	if err != nil || !made {
		t.Fatalf("made %v, %v", made, err)
	}
	if got := fake.called("NetworkLabels"); !slices.Equal(got, []string{"NetworkLabels decurix-vm-box box"}) {
		t.Fatalf("labels %v", got)
	}
	if got := fake.called("NetworkConnect"); !slices.Equal(got, []string{"NetworkConnect decurix-vm-box cid-cp"}) {
		t.Fatalf("connects %v: want the control plane alone", got)
	}
	if made, err := m.ensureDesktopNetwork(ctx, "box"); err != nil || made {
		t.Fatalf("a second call made the network again: %v %v", made, err)
	}
	if n := len(fake.called("NetworkCreate")); n != 1 {
		t.Fatalf("%d creates", n)
	}
	if _, err := m.ensureDesktopNetwork(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	// One that carries the label but not the name, and one whose label names another desktop: neither is joined.
	fake.putNetwork(network.Summary{Name: "planted", Labels: map[string]string{desktopNetworkLabel: "planted"}})
	fake.putNetwork(network.Summary{Name: DesktopNetworkName("third"), Labels: map[string]string{desktopNetworkLabel: "box"}})
	before := len(fake.called("NetworkConnect"))
	if err := m.JoinDesktopNetworks(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.called("NetworkConnect")) - before; got != 2 {
		t.Fatalf("JoinDesktopNetworks made %d connects, want one per desktop network (2): %v", got, fake.called("NetworkConnect"))
	}
}

// Each desktop network is its own /28 of the Decurix range, clear of every network there is (decurix-network's /24
// included), so stock Docker's ~30 default pools are never used up and nothing lands on the person's LAN (S1).
func TestEachDesktopNetworkIsItsOwnSmallSubnet(t *testing.T) {
	t.Setenv("DECURIX_DESKTOP_SUBNETS", "")
	fake := &fakeAPI{}
	fake.putNetwork(network.Summary{Name: NetworkName, IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.213.0.0/24"}}}})
	m := newManagerWith(fake)
	for _, id := range []string{"a", "b"} {
		if _, err := m.ensureDesktopNetwork(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.called("NetworkSubnet"); !slices.Equal(got, []string{"NetworkSubnet decurix-vm-a 10.213.1.0/28", "NetworkSubnet decurix-vm-b 10.213.1.16/28"}) {
		t.Fatalf("subnets %v", got)
	}

	// The range is the operator's to set; one with no room left is a clear error that says what to do.
	t.Setenv("DECURIX_DESKTOP_SUBNETS", "10.99.0.0/28")
	full := &fakeAPI{}
	full.putNetwork(network.Summary{Name: "x", IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.99.0.0/28"}}}})
	made, err := newManagerWith(full).ensureDesktopNetwork(context.Background(), "c")
	if made || !errors.Is(err, ErrNoNetworkAddresses) || !strings.Contains(err.Error(), "DECURIX_DESKTOP_SUBNETS") {
		t.Fatalf("made %v, err %v", made, err)
	}
	if got := freeDesktopSubnets(netip.MustParsePrefix("10.99.0.0/27"), []netip.Prefix{netip.MustParsePrefix("10.99.0.8/29")}); len(got) != 1 || got[0].String() != "10.99.0.16/28" {
		t.Fatalf("free %v", got)
	}
}

// The control plane reaches a desktop at its address on the desktop's own network; a desktop made before desktops
// had one is reached on the shared network, and own beats shared when a desktop has both.
func TestTheControlPlaneReachesADesktopOnItsOwnNetwork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		networks map[string]*network.EndpointSettings
		want     string
	}{
		{"own", map[string]*network.EndpointSettings{DesktopNetworkName("box"): {IPAddress: "10.213.4.2"}}, "10.213.4.2"},
		{"shared (older desktop)", map[string]*network.EndpointSettings{NetworkName: {IPAddress: "10.213.0.7"}}, "10.213.0.7"},
		{"both", map[string]*network.EndpointSettings{NetworkName: {IPAddress: "10.213.0.7"}, DesktopNetworkName("box"): {IPAddress: "10.213.4.2"}}, "10.213.4.2"},
		{"another desktop's", map[string]*network.EndpointSettings{DesktopNetworkName("other"): {IPAddress: "10.213.9.2"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAPI{}
			fake.inspect = func(string) (container.InspectResponse, error) {
				return container.InspectResponse{
					ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true}},
					NetworkSettings:   &container.NetworkSettings{Networks: tc.networks},
				}, nil
			}
			u, _, err := newManagerWith(fake).ProxyTarget(context.Background(), "box")
			if tc.want == "" {
				if err == nil {
					t.Fatalf("reached box at %v through another desktop's network", u)
				}
				return
			}
			if err != nil || u.Hostname() != tc.want {
				t.Fatalf("proxy target %v, %v; want %s", u, err, tc.want)
			}
		})
	}
}

// Deleting a desktop removes its own network after the control plane leaves it, and so does deleting one whose
// container is already gone (S2); a startup sweep removes a network whose desktop is gone and keeps one in use.
func TestDeletingADesktopRemovesItsNetwork(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true}}}, nil
	}
	asControlPlane(t, fake)
	m := newManagerWith(fake)
	if err := m.Delete(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	calls := fake.called("Network")
	remove := slices.Index(calls, "NetworkRemove "+DesktopNetworkName("box"))
	leave := slices.Index(calls, "NetworkDisconnect "+DesktopNetworkName("box")+" cp")
	if remove < 0 || leave < 0 || leave > remove {
		t.Fatalf("network calls %v: want the control plane to leave, then the network removed", calls)
	}

	gone := &fakeAPI{}
	if err := newManagerWith(gone).Delete(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a desktop with no container: %v", err)
	}
	if !slices.Contains(gone.called("NetworkRemove"), "NetworkRemove "+DesktopNetworkName("ghost")) {
		t.Fatalf("its network was left: %v", gone.called("Network"))
	}

	sweep := &fakeAPI{}
	sweep.putNetwork(network.Summary{Name: DesktopNetworkName("orphan"), Labels: map[string]string{desktopNetworkLabel: "orphan"}})
	sweep.putNetwork(network.Summary{Name: DesktopNetworkName("live"), Labels: map[string]string{desktopNetworkLabel: "live"}})
	sweep.inspect = func(name string) (container.InspectResponse, error) {
		if name == "desktop_live" {
			return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-live"}}, nil
		}
		return container.InspectResponse{}, errFakeNotFound
	}
	newManagerWith(sweep).SweepDesktopNetworks(context.Background())
	if got := sweep.called("NetworkRemove"); !slices.Equal(got, []string{"NetworkRemove " + DesktopNetworkName("orphan")}) {
		t.Fatalf("the sweep removed %v, want the orphan alone", got)
	}
}

// A desktop on its own network whose network the control plane cannot join is not started as if it could be
// reached (S6).
func TestADesktopTheControlPlaneCannotJoinDoesNotStart(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		if name == "cp" {
			return container.InspectResponse{}, errors.New("daemon hiccup")
		}
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{}},
			NetworkSettings:   &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{DesktopNetworkName("box"): {}}},
		}, nil
	}
	savedIn, savedHost := joinsAsContainer, controlPlaneHost
	joinsAsContainer = func() bool { return true }
	controlPlaneHost = func() (string, error) { return "cp", nil }
	t.Cleanup(func() { joinsAsContainer, controlPlaneHost = savedIn, savedHost })
	if err := newManagerWith(fake).Start(context.Background(), "box"); err == nil || !strings.Contains(err.Error(), "cannot be reached") {
		t.Fatalf("start: %v", err)
	}
	if len(fake.called("ContainerStart")) != 0 {
		t.Fatal("the desktop was started")
	}
}

// A desktop on the bundled model is given the control plane's allow-listed proxy, never the model server's own API
// (review NET-R3 B3); one on the host's own model server keeps that address.
func TestADesktopReachesTheBundledModelThroughTheProxy(t *testing.T) {
	t.Setenv("DECURIX_WINDOWS_OLLAMA_URL", "")
	t.Setenv("OLLAMA_URL", ollama.BundledURL)
	got := envValue(buildDesktopEnv("box", "thebot", "", "", Brain{Kind: BrainOllama}), "OLLAMA_URL")
	if !strings.HasSuffix(got, "/api/native-ollama") || strings.Contains(got, "decurix-ollama") {
		t.Fatalf("OLLAMA_URL=%s, want the control plane's proxy", got)
	}
	t.Setenv("OLLAMA_URL", "http://host.docker.internal:11434")
	if got := envValue(buildDesktopEnv("box", "thebot", "", "", Brain{Kind: BrainOllama}), "OLLAMA_URL"); got != "http://host.docker.internal:11434" {
		t.Fatalf("OLLAMA_URL=%s, want the host's own model server", got)
	}
}
