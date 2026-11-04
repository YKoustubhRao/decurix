package desktop

import (
	"context"
	"sort"

	"github.com/docker/docker/api/types/network"
)

// The fake daemon's networks: one exists once NetworkCreate made it (or the test put it in fakeNets) and NetworkRemove
// has not removed it since. Every call is recorded (fakeAPI.called); each connect's gateway priority is kept.
type fakeNets struct {
	byName     map[string]network.Summary
	gwPriority map[string]int // "<network> <container>" -> the priority it joined with
}

// putNetwork adds a network the daemon already has.
func (f *fakeAPI) putNetwork(n network.Summary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nets.byName == nil {
		f.nets.byName = map[string]network.Summary{}
	}
	f.nets.byName[n.Name] = n
}

func (f *fakeAPI) networkExists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.nets.byName[name]
	return ok
}

// joinedWith is the gateway priority container joined network with, and whether it joined.
func (f *fakeAPI) joinedWith(networkName, containerID string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.nets.gwPriority[networkName+" "+containerID]
	return p, ok
}

func (f *fakeAPI) NetworkInspect(_ context.Context, name string, _ network.InspectOptions) (network.Inspect, error) {
	exists := f.networkExists(name)
	f.record("NetworkInspect %s", name)
	if !exists {
		return network.Inspect{}, errFakeNotFound
	}
	return network.Inspect{Name: name}, nil
}

func (f *fakeAPI) NetworkCreate(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.record("NetworkCreate %s", name)
	f.record("NetworkLabels %s %s", name, opts.Labels[desktopNetworkLabel])
	n := network.Summary{Name: name, Driver: opts.Driver, Labels: opts.Labels}
	if opts.IPAM != nil {
		n.IPAM = *opts.IPAM
		for _, c := range opts.IPAM.Config {
			f.record("NetworkSubnet %s %s", name, c.Subnet)
		}
	}
	f.putNetwork(n)
	return network.CreateResponse{ID: "net-" + name}, nil
}

func (f *fakeAPI) NetworkConnect(_ context.Context, name, containerID string, ep *network.EndpointSettings) error {
	f.record("NetworkConnect %s %s", name, containerID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nets.gwPriority == nil {
		f.nets.gwPriority = map[string]int{}
	}
	priority := 0
	if ep != nil {
		priority = ep.GwPriority
	}
	f.nets.gwPriority[name+" "+containerID] = priority
	return nil
}

func (f *fakeAPI) NetworkDisconnect(_ context.Context, name, containerID string, _ bool) error {
	f.record("NetworkDisconnect %s %s", name, containerID)
	return nil
}

func (f *fakeAPI) NetworkRemove(_ context.Context, name string) error {
	f.record("NetworkRemove %s", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nets.byName, name)
	return nil
}

// NetworkList lists every network, by name; its filters are not applied (callers check what they get).
func (f *fakeAPI) NetworkList(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
	f.record("NetworkList")
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]network.Summary, 0, len(f.nets.byName))
	for _, n := range f.nets.byName {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
