package desktop

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// removeHalfCreated removes the container the daemon made under name although ContainerCreate answered an error (a
// timeout, a broken connection after the daemon acted): left, it held the name until a later delete (RW1-D-01). Only
// a container of exactly that name carrying this creation's id (homeCreationLabel, set before the create) is removed;
// one made by another creation, another control plane or by hand - a name conflict among them - is left alone, and so
// is anything when Docker cannot say.
func (m *Manager) removeHalfCreated(name, creation string) {
	if m == nil || m.client == nil || creation == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil || inspect.ContainerJSONBase == nil || inspect.Config == nil {
		return
	}
	if strings.TrimPrefix(inspect.Name, "/") != name || inspect.Config.Labels[homeCreationLabel] != creation || inspect.ID == "" {
		return
	}
	if err := m.client.ContainerRemove(ctx, inspect.ID, container.RemoveOptions{Force: true}); err != nil {
		log.Printf("desktop: the container %s its failed create left could not be removed: %v", name, err)
		return
	}
	log.Printf("desktop: removed the container %s its failed create had made anyway", name)
}
