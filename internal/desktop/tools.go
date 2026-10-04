package desktop

import (
	"context"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

// EnsureToolsVolume ensures the shared decurix-tools volume exists and is populated
// with the executables from decurix-tools:latest.
func (m *Manager) EnsureToolsVolume(ctx context.Context) error {
	if m.native != nil {
		return nil
	}

	m.toolsMu.Lock()
	defer m.toolsMu.Unlock()

	if m.toolsPopulated {
		return nil
	}

	_ = m.EnsureCacheVolume(ctx)

	vol, err := m.client.VolumeInspect(ctx, ToolsVolumeName)
	if err != nil || vol.Name != ToolsVolumeName {
		log.Printf("Creating shared tools volume %s...", ToolsVolumeName)
		_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{
			Name: ToolsVolumeName,
			Labels: map[string]string{
				"decurix": "true",
			},
		})
		if err != nil {
			return fmt.Errorf("create tools volume: %w", err)
		}
	}

	if _, _, err := m.client.ImageInspectWithRaw(ctx, ToolsImage); err != nil {
		log.Printf("Tools image %s not present yet; volume ready for future population", ToolsImage)
		return nil
	}

	if err := m.PopulateToolsVolume(ctx); err != nil {
		return err
	}

	m.toolsPopulated = true
	return nil
}

// PopulateToolsVolume populates the decurix-tools volume using the tools image.
func (m *Manager) PopulateToolsVolume(ctx context.Context) error {
	if m.native != nil {
		return nil
	}

	log.Printf("Populating %s from %s...", ToolsVolumeName, ToolsImage)

	containerName := fmt.Sprintf("decurix_tools_init_%s", randomHex(4))
	resp, err := m.client.ContainerCreate(
		ctx,
		&container.Config{
			Image: ToolsImage,
			Cmd:   []string{"sh", "-c", "if [ ! -f /target/.decurix-tools-ready ]; then cp -a /opt/tools/. /target/ && touch /target/.decurix-tools-ready && sync; fi"},
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

// EnsureCacheVolume ensures the shared decurix-cache volume exists and is writable
// by desktop containers to deduplicate package caches across desktops.
func (m *Manager) EnsureCacheVolume(ctx context.Context) error {
	if m.native != nil {
		return nil
	}

	vol, err := m.client.VolumeInspect(ctx, CacheVolumeName)
	if err == nil && vol.Name == CacheVolumeName {
		return nil
	}

	log.Printf("Creating shared cache volume %s...", CacheVolumeName)
	_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{
		Name: CacheVolumeName,
		Labels: map[string]string{
			"decurix": "true",
		},
	})
	if err != nil {
		return fmt.Errorf("create cache volume: %w", err)
	}

	return nil
}
