package desktop

import (
	"context"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
)

// EnsureToolsVolume ensures the shared decurix-tools volume exists and is populated
// with the executables from decurix-tools:latest.
func (m *Manager) EnsureToolsVolume(ctx context.Context) error {

	m.toolsMu.Lock()
	defer m.toolsMu.Unlock()

	if m.toolsPopulated {
		return nil
	}

	_ = m.ensureSharedVolume(ctx, HFVolumeName, "model cache")

	if err := m.ensureSharedVolume(ctx, ToolsVolumeName, "tools"); err != nil {
		return err
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

// desktopsRunning is "1" when any desktop is running (or Docker cannot say), "0" when none is.
func (m *Manager) desktopsRunning(ctx context.Context) string {
	args := filters.NewArgs()
	args.Add("label", "desktop-manager=true")
	args.Add("status", "running")
	list, err := m.client.ContainerList(ctx, container.ListOptions{Filters: args})
	if err != nil || len(list) > 0 {
		return "1"
	}
	return "0"
}

// PopulateToolsVolume populates the decurix-tools volume using the tools image.
func (m *Manager) PopulateToolsVolume(ctx context.Context) error {

	log.Printf("Populating %s from %s...", ToolsVolumeName, ToolsImage)

	containerName := fmt.Sprintf("decurix_tools_init_%s", randomHex(4))
	resp, err := m.client.ContainerCreate(
		ctx,
		&container.Config{
			// No Cmd: the tools image's own CMD is the copy rule (docker/tools.Dockerfile), so it lives in one place
			// and a rebuilt image with a new sentinel replaces what the volume holds (B-24). It is told whether
			// desktops are running: a refresh then waits, since their /opt/tools would vanish mid-task (RW1-D-02).
			Image: ToolsImage,
			Env:   []string{"DECURIX_DESKTOPS_RUNNING=" + m.desktopsRunning(ctx)},
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

// ensureSharedVolume creates one of the volumes every desktop shares read-only (the tools, the model cache) unless it
// exists; what names it in the log and errors.
func (m *Manager) ensureSharedVolume(ctx context.Context, name, what string) error {

	vol, err := m.client.VolumeInspect(ctx, name)
	if err == nil && vol.Name == name {
		return nil
	}

	log.Printf("Creating shared %s volume %s...", what, name)
	_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			"decurix": "true",
		},
	})
	if err != nil {
		return fmt.Errorf("create %s volume: %w", what, err)
	}

	return nil
}
