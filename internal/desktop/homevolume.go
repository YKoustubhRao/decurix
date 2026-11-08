package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// A desktop's home volume carries the id of the creation that made it, so a creation knows whether a volume of its
// name is its own (B-03). One left by an earlier desktop of the same name (its delete could not remove it) is not
// silently adopted: that would hand the new desktop the old one's memory and sign-ins.

const (
	// DecurixLabel marks every Docker resource Decurix makes for itself, so cleanup can find exactly those.
	DecurixLabel = "decurix"
	// homeDesktopLabel is the desktop a home volume belongs to.
	homeDesktopLabel = "decurix.desktop"
	// homeCreationLabel is the creation that made a home volume.
	homeCreationLabel = "decurix.creation"
)

// newCreationID is a fresh, unguessable id for one desktop creation.
func newCreationID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// alreadyExists is the error for a desktop name that is taken, naming the id the name was turned into when that
// differs, since two names can make one id ("My Box" and "my-box") (B-36).
func alreadyExists(name, id string) error {
	if name != id {
		return fmt.Errorf("a desktop called %s already exists (the name %q is stored as %s); choose another name", id, name, id)
	}
	return fmt.Errorf("desktop %s already exists", name)
}

// homeVolumeFor gets creation's home volume called name for desktop id: a new one, labelled as this creation's
// (made is true, and only then may a failed creation remove it), or an old unlabelled one, which is adopted as before
// volumes were labelled. A volume another creation made is refused, and so is any answer from Docker that is not "no
// such volume".
func (m *Manager) homeVolumeFor(ctx context.Context, name, id, creation string) (made bool, err error) {
	existing, err := m.client.VolumeInspect(ctx, name)
	switch {
	case err == nil:
		if owner := existing.Labels[homeCreationLabel]; owner != "" && owner != creation {
			return false, leftoverError(fmt.Sprintf("the home folder of an earlier desktop called %s is still there "+
				"(volume %s); it is not reused for a new desktop: create it again from the dashboard, which offers to "+
				"delete that old home, or choose another name", id, name))
		}
		log.Printf("desktop %s: reusing its existing home volume %s (made before home volumes were labelled)", id, name)
		return false, nil
	case !client.IsErrNotFound(err):
		return false, fmt.Errorf("check the home volume: %w", err)
	}
	created, err := m.client.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{
		DecurixLabel: "true", homeDesktopLabel: id, homeCreationLabel: creation,
	}})
	if err != nil {
		return false, fmt.Errorf("create volume: %w", err)
	}
	if created.Labels[homeCreationLabel] != creation {
		// Docker hands back a volume of that name that appeared meanwhile, labels and all: not ours.
		return false, fmt.Errorf("the home volume %s appeared while this desktop was being made; try again", name)
	}
	return true, nil
}

const (
	// HFVolumeName is the model cache every desktop shares read-only, at HFMountPoint: models the control plane puts
	// there once, so no desktop downloads its own copy and no desktop can change another's (B-02, owner decision O-2).
	HFVolumeName = "decurix-models"
	// HFMountPoint is where a desktop sees HFVolumeName (DECURIX_SHARED_MODELS); HF_HOME stays its own cache.
	HFMountPoint = "/var/cache/decurix-models"
)

// cacheVolumeFor is the package-cache volume (pip, npm, Go modules, HF_HOME) of the desktop container called
// containerName: one per desktop, deleted with it.
func cacheVolumeFor(containerName string) string { return containerName + "_cache" }

// cacheVolumeFresh makes desktop id's cache volume name fresh for creation: one a deleted desktop of the same name
// left is removed first (a cache is disposable, and another worker wrote it), then a new one is made and labelled.
// made is true once this creation owns it.
func (m *Manager) cacheVolumeFresh(ctx context.Context, name, id, creation string) (made bool, err error) {
	if existing, err := m.client.VolumeInspect(ctx, name); err == nil {
		if existing.Labels[homeCreationLabel] == creation {
			return true, nil
		}
		if err := m.client.VolumeRemove(ctx, name, false); err != nil {
			return false, fmt.Errorf("an earlier desktop's package cache %s is still there and in use: %w", name, err)
		}
	} else if !client.IsErrNotFound(err) {
		return false, fmt.Errorf("check the package cache volume: %w", err)
	}
	if _, err := m.client.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{
		DecurixLabel: "true", homeDesktopLabel: id, homeCreationLabel: creation,
	}}); err != nil {
		return false, fmt.Errorf("create the package cache volume: %w", err)
	}
	return true, nil
}

// A delete whose volume removal failed leaves the desktop's home behind, and a new desktop of that name is then
// refused (homeVolumeFor). LeftoverHome lets the create form say so before anything starts, and RemoveLeftoverHome is
// the person's way out: it removes only that home, only once no desktop of the name exists, and never a shared volume.

var (
	// ErrLeftoverHome is a home volume an earlier desktop of the same name left, which a new one does not adopt.
	ErrLeftoverHome = errors.New("an earlier desktop's home folder is still there")
	// ErrNoLeftoverHome is a name with no home volume left by an earlier desktop: nothing to remove.
	ErrNoLeftoverHome = errors.New("no home folder of an earlier desktop is left under that name")
	// ErrHomeInUse is a leftover home that something still has: a desktop of that name, a creation, a container.
	ErrHomeInUse = errors.New("that home folder is still in use")
)

// leftoverError is the creation's refusal of a leftover home, in words for the person; it is ErrLeftoverHome.
type leftoverError string

// Error is the refusal's words.
func (e leftoverError) Error() string { return string(e) }

// Is makes a leftoverError ErrLeftoverHome.
func (e leftoverError) Is(target error) bool { return target == ErrLeftoverHome }

// homeVolumeName is the name of desktop id's home volume.
func homeVolumeName(id string) string { return fmt.Sprintf("desktop_%s_home", id) }

// sharedVolume reports whether name is a volume every desktop shares, which nothing per desktop may remove.
func sharedVolume(name string) bool {
	return name == ToolsVolumeName || name == HFVolumeName || name == CacheVolumeName
}

// LeftoverHome reports whether a new desktop called name would be refused for a home volume an earlier desktop of
// that name left: one an earlier creation labelled, with no desktop container of that name (a desktop that is there
// is "already exists", not this).
func (m *Manager) LeftoverHome(ctx context.Context, name string) (bool, error) {
	id := sanitizeName(name)
	if id == "" {
		return false, nil
	}
	if _, err := m.client.ContainerInspect(ctx, "desktop_"+id); err == nil {
		return false, nil
	} else if !client.IsErrNotFound(err) {
		return false, fmt.Errorf("check desktop %s: %w", id, err)
	}
	existing, err := m.client.VolumeInspect(ctx, homeVolumeName(id))
	if client.IsErrNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check the home volume of %s: %w", id, err)
	}
	return leftoverLabels(existing, id), nil
}

// leftoverLabels reports whether v is a home volume Decurix made for a desktop called id: labelled with a creation,
// and, when it names one, that desktop.
func leftoverLabels(v volume.Volume, id string) bool {
	if v.Labels[homeCreationLabel] == "" {
		return false
	}
	owner := v.Labels[homeDesktopLabel]
	return owner == "" || owner == id
}

// RemoveLeftoverHome removes the home volume an earlier desktop called name left - its files, memory and sign-ins,
// for good - so a new desktop of that name can be made. It is refused (ErrHomeInUse) while a desktop of that name
// exists or is being created, or while any container still mounts it; a name with no such volume, or one Decurix did
// not label as a desktop's home, is ErrNoLeftoverHome. A shared volume is never touched.
func (m *Manager) RemoveLeftoverHome(ctx context.Context, name string) error {
	id := sanitizeName(name)
	if id == "" {
		return ErrNoLeftoverHome
	}
	volName := homeVolumeName(id)
	if sharedVolume(volName) {
		return ErrNoLeftoverHome
	}
	if m.creating(id) {
		return fmt.Errorf("%w: desktop %s is being created", ErrHomeInUse, id)
	}
	if _, err := m.client.ContainerInspect(ctx, "desktop_"+id); err == nil {
		return fmt.Errorf("%w: desktop %s exists; delete it from its card", ErrHomeInUse, id)
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("check desktop %s: %w", id, err)
	}
	existing, err := m.client.VolumeInspect(ctx, volName)
	if client.IsErrNotFound(err) {
		return ErrNoLeftoverHome
	}
	if err != nil {
		return fmt.Errorf("check the home volume of %s: %w", id, err)
	}
	if !leftoverLabels(existing, id) {
		return ErrNoLeftoverHome
	}

	// A home-copy helper the old desktop's delete did not get to would hold it; nothing else is this call's to stop.
	m.removeHomeHelpersOf(ctx, "desktop_"+id)
	args := filters.NewArgs()
	args.Add("volume", volName)
	users, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return fmt.Errorf("check what uses %s: %w", volName, err)
	}
	for _, c := range users {
		for _, mp := range c.Mounts {
			if mp.Name == volName {
				return fmt.Errorf("%w: container %s mounts %s", ErrHomeInUse, c.ID, volName)
			}
		}
	}
	// A creation that began meanwhile has claimed the name: its volume is not an earlier desktop's.
	if m.creating(id) {
		return fmt.Errorf("%w: desktop %s is being created", ErrHomeInUse, id)
	}
	// Never forced: Docker itself refuses a volume a container still uses.
	if err := m.client.VolumeRemove(ctx, volName, false); err != nil {
		if errdefs.IsConflict(err) {
			return fmt.Errorf("%w: %v", ErrHomeInUse, err)
		}
		if client.IsErrNotFound(err) {
			return ErrNoLeftoverHome
		}
		return fmt.Errorf("remove %s: %w", volName, err)
	}
	log.Printf("desktop %s: removed the home volume %s an earlier desktop of that name left, as a person asked", id, volName)
	return nil
}

// creating reports whether a creation of desktop id is running now (a failed one kept for the page is not).
func (m *Manager) creating(id string) bool {
	m.provisioningMu.RLock()
	defer m.provisioningMu.RUnlock()
	prov, ok := m.provisioning[id]
	if !ok {
		return false
	}
	_, running := m.provisioningRuns[prov]
	return running
}
