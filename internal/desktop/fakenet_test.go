package desktop

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/network"
)

// The fake daemon's networks: a network exists once NetworkCreate was called for it and NetworkRemove was not since.
// Every call is recorded (fakeAPI.called).

func (f *fakeAPI) networkExists(name string) bool {
	exists := false
	for _, c := range f.called("Network") {
		switch c {
		case "NetworkCreate " + name:
			exists = true
		case "NetworkRemove " + name:
			exists = false
		}
	}
	return exists
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
	return network.CreateResponse{ID: "net-" + name}, nil
}

func (f *fakeAPI) NetworkConnect(_ context.Context, name, containerID string, _ *network.EndpointSettings) error {
	f.record("NetworkConnect %s %s", name, containerID)
	return nil
}

func (f *fakeAPI) NetworkDisconnect(_ context.Context, name, containerID string, _ bool) error {
	f.record("NetworkDisconnect %s %s", name, containerID)
	return nil
}

func (f *fakeAPI) NetworkRemove(_ context.Context, name string) error {
	f.record("NetworkRemove %s", name)
	return nil
}

func (f *fakeAPI) NetworkList(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
	f.record("NetworkList")
	var out []network.Summary
	seen := map[string]bool{}
	for _, c := range f.called("NetworkCreate ") {
		name := strings.TrimPrefix(c, "NetworkCreate ")
		if !seen[name] && f.networkExists(name) {
			seen[name] = true
			out = append(out, network.Summary{Name: name})
		}
	}
	return out, nil
}
