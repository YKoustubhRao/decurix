package desktop

import (
	"context"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

// EnsureToolsVolume ensures the shared flowera-tools volume exists and is populated
// with the executables from flowera-tools:latest.
func (m *Manager) EnsureToolsVolume(ctx context.Context) error {
	_ = m.EnsureCacheVolume(ctx)

	vol, err := m.client.VolumeInspect(ctx, ToolsVolumeName)
	if err == nil && vol.Name == ToolsVolumeName {
		return nil
	}

	log.Printf("Creating shared tools volume %s...", ToolsVolumeName)
	_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{
		Name: ToolsVolumeName,
		Labels: map[string]string{
			"flowera": "true",
		},
	})
	if err != nil {
		return fmt.Errorf("create tools volume: %w", err)
	}

	if _, _, err := m.client.ImageInspectWithRaw(ctx, ToolsImage); err != nil {
		log.Printf("Tools image %s not present yet; volume created empty", ToolsImage)
		return nil
	}

	return m.PopulateToolsVolume(ctx)
}

// PopulateToolsVolume populates the flowera-tools volume using the tools image.
func (m *Manager) PopulateToolsVolume(ctx context.Context) error {
	log.Printf("Populating %s from %s...", ToolsVolumeName, ToolsImage)

	containerName := fmt.Sprintf("flowera_tools_init_%s", randomHex(4))
	resp, err := m.client.ContainerCreate(
		ctx,
		&container.Config{
			Image: ToolsImage,
		},
		&container.HostConfig{
			Binds: []string{
				ToolsVolumeName + ":/target",
			},
		},
		nil,
		nil,
		containerName,
	)
	if err != nil {
		return fmt.Errorf("create tools init container: %w", err)
	}
	defer func() {
		_ = m.client.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	}()

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start tools init container: %w", err)
	}

	statusCh, errCh := m.client.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wait for tools init: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("tools init container exited with status %d", status.StatusCode)
		}
	}

	log.Printf("Shared tools volume %s populated successfully.", ToolsVolumeName)
	return nil
}

// EnsureCacheVolume ensures the shared flowera-cache volume exists and is writable
// by desktop containers to deduplicate package caches across desktops.
func (m *Manager) EnsureCacheVolume(ctx context.Context) error {
	vol, err := m.client.VolumeInspect(ctx, CacheVolumeName)
	if err == nil && vol.Name == CacheVolumeName {
		return nil
	}

	log.Printf("Creating shared cache volume %s...", CacheVolumeName)
	_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{
		Name: CacheVolumeName,
		Labels: map[string]string{
			"flowera": "true",
		},
	})
	if err != nil {
		return fmt.Errorf("create cache volume: %w", err)
	}

	return nil
}
