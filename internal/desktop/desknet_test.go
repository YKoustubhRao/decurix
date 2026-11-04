package desktop

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
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

// Making a desktop's network labels it with the desktop and joins the model server to it; a second call makes
// nothing; JoinDesktopNetworks (a control plane or model server made again) joins every desktop network there is.
func TestADesktopNetworkIsMadeOnceAndJoined(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		if name == "decurix-ollama" {
			return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-ollama"}, NetworkSettings: &container.NetworkSettings{}}, nil
		}
		return container.InspectResponse{}, errFakeNotFound
	}
	m := newManagerWith(fake)
	m.ModelServer = "decurix-ollama"
	ctx := context.Background()
	made, err := m.ensureDesktopNetwork(ctx, "box")
	if err != nil || !made {
		t.Fatalf("made %v, %v", made, err)
	}
	if got := fake.called("NetworkLabels"); !slices.Equal(got, []string{"NetworkLabels decurix-desktop-box box"}) {
		t.Fatalf("labels %v", got)
	}
	if !slices.Contains(fake.called("NetworkConnect"), "NetworkConnect decurix-desktop-box cid-ollama") {
		t.Fatalf("the model server did not join: %v", fake.called("NetworkConnect"))
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
	before := len(fake.called("NetworkConnect"))
	m.JoinDesktopNetworks(ctx)
	if got := len(fake.called("NetworkConnect")) - before; got != 2 {
		t.Fatalf("JoinDesktopNetworks made %d connects, want one per desktop network (2)", got)
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

// Deleting a desktop removes its own network, after the control plane and the model server leave it.
func TestDeletingADesktopRemovesItsNetwork(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true}}}, nil
	}
	m := newManagerWith(fake)
	m.ModelServer = "decurix-ollama"
	if err := m.Delete(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	calls := fake.called("Network")
	remove := slices.Index(calls, "NetworkRemove "+DesktopNetworkName("box"))
	leave := slices.Index(calls, "NetworkDisconnect "+DesktopNetworkName("box")+" decurix-ollama")
	if remove < 0 || leave < 0 || leave > remove {
		t.Fatalf("network calls %v: want the model server to leave, then the network removed", calls)
	}
}

// No address is read from a container's networks but through desktopEndpoint (desknet.go), so none reads the shared
// network directly: an AST walk of every non-test Go file of the control plane. The practice network is read by its
// own code, by its own name.
func TestNoAddressIsReadFromTheSharedNetworkDirectly(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			allowed := filepath.Base(p) == "desknet.go" && filepath.Base(filepath.Dir(p)) == "desktop"
			ast.Inspect(file, func(n ast.Node) bool {
				idx, ok := n.(*ast.IndexExpr)
				if !ok {
					return true
				}
				sel, ok := idx.X.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Networks" {
					return true
				}
				if key, ok := idx.Index.(*ast.Ident); ok && key.Name == "PracticeNetworkName" {
					return true
				}
				if !allowed {
					t.Errorf("%s: a container's address read outside desktopEndpoint", fset.Position(idx.Pos()))
				}
				return true
			})
			return nil
		})
	}
}

// poolExhausted is a daemon with no address range left for another network.
type poolExhausted struct{ *fakeAPI }

func (poolExhausted) NetworkCreate(context.Context, string, network.CreateOptions) (network.CreateResponse, error) {
	return network.CreateResponse{}, errorString("Error response from daemon: could not find an available, non-overlapping IPv4 address pool among the defaults to assign to the network")
}

type errorString string

func (e errorString) Error() string { return string(e) }

// A daemon with no range left for another desktop's network is said in plain words (ErrNoNetworkAddresses), with
// what to do, and nothing is made.
func TestNoAddressRangeLeftIsAClearError(t *testing.T) {
	m := newManagerWith(poolExhausted{&fakeAPI{}})
	made, err := m.ensureDesktopNetwork(context.Background(), "box")
	if made || !errors.Is(err, ErrNoNetworkAddresses) || !strings.Contains(err.Error(), "default-address-pools") {
		t.Fatalf("made %v, err %v", made, err)
	}
}
