package desktop

// Tests for a home volume an earlier desktop of the same name left (LeftoverHome, RemoveLeftoverHome): the create
// form asks first, and the person's Delete removes only that home, only once nothing of that desktop is left.

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

// leftoverFake is a daemon holding desktop "box"'s home volume with labels, and no container unless inspect says.
func leftoverFake(labels map[string]string) *fakeAPI {
	return &fakeAPI{volumeGet: func(name string) (volume.Volume, error) {
		if name == "desktop_box_home" {
			return volume.Volume{Name: name, Labels: labels}, nil
		}
		return volume.Volume{}, errFakeNotFound
	}}
}

var earlierCreation = map[string]string{DecurixLabel: "true", homeDesktopLabel: "box", homeCreationLabel: "earlier"}

func TestALeftoverHomeIsFoundAndRemoved(t *testing.T) {
	fake := leftoverFake(earlierCreation)
	m := provisioningManager(fake)

	leftover, err := m.LeftoverHome(context.Background(), "Box")
	if err != nil || !leftover {
		t.Fatalf("LeftoverHome = %v, %v; want true", leftover, err)
	}
	if err := m.RemoveLeftoverHome(context.Background(), "Box"); err != nil {
		t.Fatal(err)
	}
	if got := fake.called("VolumeRemove"); len(got) != 1 || got[0] != "VolumeRemove desktop_box_home" {
		t.Fatalf("removed %v, want only the home volume", got)
	}
}

func TestALeftoverHomeIsKeptWhileItsDesktopExists(t *testing.T) {
	fake := leftoverFake(earlierCreation)
	fake.inspect = func(name string) (container.InspectResponse, error) { return container.InspectResponse{}, nil }
	m := provisioningManager(fake)

	if leftover, _ := m.LeftoverHome(context.Background(), "box"); leftover {
		t.Fatal("a desktop that is there is not a leftover home")
	}
	if err := m.RemoveLeftoverHome(context.Background(), "box"); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("err = %v, want ErrHomeInUse", err)
	}
	if got := fake.called("VolumeRemove"); len(got) > 0 {
		t.Fatalf("a desktop's own home was removed: %v", got)
	}
}

func TestOnlyADesktopsLabelledHomeIsRemoved(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"unlabelled (adopted, not refused)": nil,
		"another desktop's":                 {homeDesktopLabel: "other", homeCreationLabel: "earlier"},
	} {
		fake := leftoverFake(labels)
		m := provisioningManager(fake)
		if err := m.RemoveLeftoverHome(context.Background(), "box"); !errors.Is(err, ErrNoLeftoverHome) {
			t.Errorf("%s: err = %v, want ErrNoLeftoverHome", name, err)
		}
		if got := fake.called("VolumeRemove"); len(got) > 0 {
			t.Errorf("%s: removed %v", name, got)
		}
	}

	m := provisioningManager(&fakeAPI{})
	if err := m.RemoveLeftoverHome(context.Background(), "nothing-here"); !errors.Is(err, ErrNoLeftoverHome) {
		t.Fatalf("no volume: err = %v, want ErrNoLeftoverHome", err)
	}
}

func TestALeftoverHomeAContainerMountsIsKept(t *testing.T) {
	fake := leftoverFake(earlierCreation)
	fake.containers = []container.Summary{{ID: "c1", Mounts: []container.MountPoint{{Name: "desktop_box_home"}}}}
	m := provisioningManager(fake)

	if err := m.RemoveLeftoverHome(context.Background(), "box"); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("err = %v, want ErrHomeInUse", err)
	}
	if got := fake.called("VolumeRemove"); len(got) > 0 {
		t.Fatalf("a mounted home was removed: %v", got)
	}
}

func TestALeftoverHomeIsKeptWhileADesktopOfItsNameIsBeingMade(t *testing.T) {
	fake := leftoverFake(earlierCreation)
	m := provisioningManager(fake)
	prov := &ProvisioningDesktop{ID: "box"}
	m.provisioning["box"] = prov
	m.provisioningRuns = map[*ProvisioningDesktop]provisioningRun{prov: {}}

	if err := m.RemoveLeftoverHome(context.Background(), "box"); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("err = %v, want ErrHomeInUse", err)
	}
	if got := fake.called("VolumeRemove"); len(got) > 0 {
		t.Fatalf("removed %v during a creation", got)
	}
}

func TestTheSharedVolumesAreNeverADesktopsHome(t *testing.T) {
	for _, name := range []string{ToolsVolumeName, HFVolumeName, CacheVolumeName} {
		if !sharedVolume(name) {
			t.Errorf("%s is not treated as shared", name)
		}
	}
	if sharedVolume(homeVolumeName("box")) {
		t.Error("a desktop's home is treated as shared")
	}
}

// The creation's own refusal carries ErrLeftoverHome, so a caller can tell it from any other failure.
func TestTheCreationRefusalIsErrLeftoverHome(t *testing.T) {
	m := provisioningManager(leftoverFake(earlierCreation))
	_, err := m.homeVolumeFor(context.Background(), "desktop_box_home", "box", "this-one")
	if !errors.Is(err, ErrLeftoverHome) {
		t.Fatalf("err = %v, want ErrLeftoverHome", err)
	}
}
