package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
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
			return false, fmt.Errorf("the home folder of an earlier desktop called %s is still there (volume %s); "+
				"delete that volume or choose another name - it is not reused for a new desktop", id, name)
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
