package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

// provisioningManager is a Manager over fake whose tools volume and seccomp answer are already settled, so a creation
// goes straight to the steps the tests look at.
func provisioningManager(fake *fakeAPI) *Manager {
	m := newManagerWith(fake)
	m.HostGUISecret = []byte("secret")
	m.toolsPopulated = true
	m.secOptResolved = true
	return m
}

// waitProvisioned waits for the creation of snapshot's desktop to end in failure, and returns its live record.
func waitProvisioned(t *testing.T, m *Manager, snapshot *ProvisioningDesktop) *ProvisioningDesktop {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.provisioningMu.RLock()
		prov := m.provisioning[snapshot.ID]
		status, stage := "", ""
		running := false
		if prov != nil {
			status, stage = prov.Status, prov.Stage
			_, running = m.provisioningRuns[prov]
		}
		m.provisioningMu.RUnlock()
		if status == "failed" && !running {
			return prov
		}
		if time.Now().After(deadline) {
			t.Fatalf("creation did not end: %s %s", status, stage)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAnInspectThatTimesOutRemovesNothing: an existing desktop whose inspect timed out was taken as absent; the create
// then failed on the name, and its cleanup removed the existing desktop and its home (B-03). Now nothing is touched.
func TestAnInspectThatTimesOutRemovesNothing(t *testing.T) {
	fake := &fakeAPI{inspect: func(string) (container.InspectResponse, error) {
		return container.InspectResponse{}, context.DeadlineExceeded
	}}
	m := provisioningManager(fake)
	_, err := m.BeginProvisioning("box", "basic", "thebot", Brain{}, 1, 2048, "", "", "", nil, nil, nil)
	if err == nil {
		t.Fatal("a creation must not go on when Docker did not say the desktop is absent")
	}
	time.Sleep(20 * time.Millisecond)
	if c := append(fake.called("ContainerRemove"), fake.called("VolumeRemove")...); len(c) > 0 {
		t.Fatalf("removed %v", c)
	}
	if c := fake.called("ContainerCreate"); len(c) > 0 {
		t.Fatalf("created %v", c)
	}
}

// TestANameConflictRemovesNothingThatWasThere: the desktop appeared between the check and the create (the create
// fails on the name); the cleanup removes only what this creation made - here, nothing, as the home was an old
// unlabelled one it reused.
func TestANameConflictRemovesNothingThatWasThere(t *testing.T) {
	fake := &fakeAPI{
		volumeGet: func(name string) (volume.Volume, error) {
			if strings.HasSuffix(name, "_home") {
				return volume.Volume{Name: name}, nil
			}
			return volume.Volume{}, errFakeNotFound
		},
		create: func(name string, _ *container.Config, _ *container.HostConfig) (container.CreateResponse, error) {
			return container.CreateResponse{}, errors.New("Conflict. The container name is already in use")
		},
	}
	m := provisioningManager(fake)
	prov, err := m.BeginProvisioning("box", "basic", "thebot", Brain{}, 1, 2048, "", "", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitProvisioned(t, m, prov)
	// Only the package cache this creation made itself may go.
	if c := append(fake.called("ContainerRemove"), fake.called("VolumeRemove desktop_box_home")...); len(c) > 0 {
		t.Fatalf("a failed creation removed what it did not make: %v", c)
	}
}

// TestAFailedCreationRemovesTheHomeItMade: what a creation made itself is cleaned up when it fails.
func TestAFailedCreationRemovesTheHomeItMade(t *testing.T) {
	fake := &fakeAPI{create: func(string, *container.Config, *container.HostConfig) (container.CreateResponse, error) {
		return container.CreateResponse{}, errors.New("no space left")
	}}
	m := provisioningManager(fake)
	prov, err := m.BeginProvisioning("box", "basic", "thebot", Brain{}, 1, 2048, "", "", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitProvisioned(t, m, prov)
	if c := fake.called("VolumeRemove desktop_box_home"); len(c) != 1 {
		t.Fatalf("the home this creation made must be removed: %v", fake.calls)
	}
	if c := fake.called("ContainerRemove"); len(c) > 0 {
		t.Fatalf("no container was made, so none may be removed: %v", c)
	}
}

// TestAnEarlierDesktopsHomeIsNotAdopted: a home volume labelled by another creation (an earlier desktop of this name
// whose delete left it) is refused, and left alone.
func TestAnEarlierDesktopsHomeIsNotAdopted(t *testing.T) {
	fake := &fakeAPI{volumeGet: func(name string) (volume.Volume, error) {
		return volume.Volume{Name: name, Labels: map[string]string{homeCreationLabel: "someone-else"}}, nil
	}}
	m := provisioningManager(fake)
	prov, err := m.BeginProvisioning("box", "basic", "thebot", Brain{}, 1, 2048, "", "", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prov = waitProvisioned(t, m, prov)
	if !strings.Contains(prov.Error, "earlier desktop") {
		t.Fatalf("error = %q", prov.Error)
	}
	if c := append(fake.called("ContainerCreate"), fake.called("VolumeRemove")...); len(c) > 0 {
		t.Fatalf("an earlier desktop's home must be neither used nor removed: %v", c)
	}
}

// TestANewHomeIsLabelledWithItsCreation.
func TestANewHomeIsLabelledWithItsCreation(t *testing.T) {
	var got map[string]string
	fake := &fakeAPI{volumeCreate: func(o volume.CreateOptions) (volume.Volume, error) {
		got = o.Labels
		return volume.Volume{Name: o.Name, Labels: o.Labels}, nil
	}}
	m := newManagerWith(fake)
	made, err := m.homeVolumeFor(context.Background(), "desktop_box_home", "box", "c1")
	if err != nil || !made {
		t.Fatalf("made %v, %v", made, err)
	}
	if got[homeCreationLabel] != "c1" || got[homeDesktopLabel] != "box" || got[DecurixLabel] != "true" {
		t.Fatalf("labels %v", got)
	}
}

// TestTheTakenNameNamesItsID: two names can make one id; the person is told which (B-36).
func TestTheTakenNameNamesItsID(t *testing.T) {
	if err := alreadyExists("My Box", "my-box"); !strings.Contains(err.Error(), "my-box") || !strings.Contains(err.Error(), "My Box") {
		t.Fatal(err)
	}
}

// TestDeleteForgetsTheSizeItWasGiven: a desktop made again under a deleted one's name starts at its own size (B-25).
func TestDeleteForgetsTheSizeItWasGiven(t *testing.T) {
	fake := &fakeAPI{inspect: func(name string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid", State: &container.State{}}}, nil
	}}
	m := newManagerWith(fake)
	m.resourceOverrides["box"] = ResourceAllocation{CPUs: 4, MemoryMB: 8192}
	if err := m.Delete(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.resourceOverrides["box"]; ok {
		t.Fatal("the deleted desktop's size outlived it")
	}
}
