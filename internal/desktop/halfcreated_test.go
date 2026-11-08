package desktop

import (
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// RW1-D-01: the daemon made the container although ContainerCreate answered an error. The creation removes it - the
// one of that exact name carrying its own creation id - and nothing of another's.
func TestAFailedCreateRemovesTheContainerItMadeAnyway(t *testing.T) {
	var labels map[string]string
	fake := &fakeAPI{}
	fake.create = func(name string, cfg *container.Config, _ *container.HostConfig) (container.CreateResponse, error) {
		labels = cfg.Labels
		return container.CreateResponse{}, errors.New("context deadline exceeded")
	}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		if name != "desktop_box" || labels == nil {
			return container.InspectResponse{}, errFakeNotFound
		}
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "made-anyway", Name: "/desktop_box"},
			Config: &container.Config{Labels: labels}}, nil
	}
	m := provisioningManager(fake)
	prov, err := m.BeginProvisioning("box", "basic", Brain{}, 1, 2048, "", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitProvisioned(t, m, prov)
	if labels[homeCreationLabel] == "" {
		t.Fatalf("the container is not labelled with its creation: %v", labels)
	}
	if got := fake.called("ContainerRemove"); len(got) != 1 || got[0] != "ContainerRemove made-anyway" {
		t.Fatalf("removed %v, want only the container this creation made", got)
	}
}

// A container of that name another creation (or another control plane, or a person) made between the creation's check
// and its create is left alone, and so is one whose name only resembles it.
func TestAFailedCreateLeavesAnothersContainer(t *testing.T) {
	for name, found := range map[string]container.InspectResponse{
		"another creation's": {ContainerJSONBase: &container.ContainerJSONBase{ID: "theirs", Name: "/desktop_box"},
			Config: &container.Config{Labels: map[string]string{homeCreationLabel: "someone-else"}}},
		"unlabelled": {ContainerJSONBase: &container.ContainerJSONBase{ID: "theirs", Name: "/desktop_box"},
			Config: &container.Config{Labels: map[string]string{}}},
		"another name": {ContainerJSONBase: &container.ContainerJSONBase{ID: "theirs", Name: "/desktop_box2"},
			Config: &container.Config{Labels: map[string]string{}}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAPI{}
			found, there := found, false // there once the create ran: it came between the check and the create
			fake.create = func(string, *container.Config, *container.HostConfig) (container.CreateResponse, error) {
				fake.mu.Lock()
				there = true
				fake.mu.Unlock()
				return container.CreateResponse{}, errors.New("Conflict. The container name is already in use")
			}
			fake.inspect = func(n string) (container.InspectResponse, error) {
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if n == "desktop_box" && there {
					return found, nil
				}
				return container.InspectResponse{}, errFakeNotFound
			}
			m := provisioningManager(fake)
			prov, err := m.BeginProvisioning("box", "basic", Brain{}, 1, 2048, "", "", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			waitProvisioned(t, m, prov)
			if got := fake.called("ContainerRemove"); len(got) > 0 {
				t.Fatalf("removed %v: not this creation's", got)
			}
		})
	}
}
