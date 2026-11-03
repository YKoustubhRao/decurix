package desktop

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
)

// EnsureToolsVolume ensures the shared decurix-tools volume exists and is filled from decurix-tools:latest as it is
// now. It is called at start, at each desktop's creation and when the last desktop stops: whenever the tools image
// is not the one the volume was last filled from (a tools image built after start, F-T), it fills the volume again,
// or (desktops running) says the refresh waits; when it is, it does nothing.
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

	img, _, err := m.client.ImageInspectWithRaw(ctx, ToolsImage)
	if err != nil {
		log.Printf("Tools image %s not present yet; volume ready for future population", ToolsImage)
		return nil
	}
	if img.ID != "" && img.ID == m.toolsApplied && !m.toolsWaiting.Load() {
		return nil
	}

	return m.populateFrom(ctx, img.ID)
}

// populateFrom is PopulateToolsVolume, noting the tools image it applied (EnsureToolsVolume then has nothing left
// to do until the image changes). The caller holds toolsMu.
func (m *Manager) populateFrom(ctx context.Context, imageID string) error {
	if err := m.PopulateToolsVolume(ctx); err != nil {
		return err
	}
	if !m.toolsWaiting.Load() {
		m.toolsApplied = imageID
	}
	return nil
}

// populateToolsLocked is PopulateToolsVolume under toolsMu, noting whether it applied or waits for running desktops.
func (m *Manager) populateToolsLocked(ctx context.Context) error {
	m.toolsMu.Lock()
	defer m.toolsMu.Unlock()
	imageID := ""
	if img, _, err := m.client.ImageInspectWithRaw(ctx, ToolsImage); err == nil {
		imageID = img.ID
	}
	return m.populateFrom(ctx, imageID)
}

// refreshToolsWhenIdle tries a waiting tools refresh again once a desktop has stopped: if it was the last one
// running, the refresh applies now rather than at the next creation. In the background; a failure is logged.
func (m *Manager) refreshToolsWhenIdle() {
	if !m.toolsWaiting.Load() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := m.EnsureToolsVolume(ctx); err != nil {
			log.Printf("Shared tools volume %s: the waiting refresh failed: %v", ToolsVolumeName, err)
		}
	}()
}

// toolsWaitExit is the tools image's exit status when it left the volume as it was because desktops were running
// (docker/tools.Dockerfile).
const toolsWaitExit = 75

// ToolsRefreshWaiting reports whether a newer tools image is waiting for the running desktops to stop before it
// replaces the shared tools volume (REVIEW-3 follow-up 2): a populate said so, or the tools image is no longer the
// one the volume was filled from (built after start, F-T) and has not been applied yet.
func (m *Manager) ToolsRefreshWaiting(ctx context.Context) bool {
	if m.toolsWaiting.Load() {
		return true
	}
	m.toolsMu.Lock()
	applied, skip := m.toolsApplied, m.toolsPopulated
	m.toolsMu.Unlock()
	if skip || applied == "" {
		return false
	}
	img, _, err := m.client.ImageInspectWithRaw(ctx, ToolsImage)
	return err == nil && img.ID != "" && img.ID != applied
}

// desktopsRunning is "1" when any desktop may use /opt/tools now or on resume (or Docker cannot say), "0" when none
// may. Paused counts: desktops are idle-paused routinely, and one resumed mid-task would find its tools replaced
// (RW1-G-02). So does restarting, and any state but exited, created or dead.
func (m *Manager) desktopsRunning(ctx context.Context) string {
	args := filters.NewArgs()
	args.Add("label", "desktop-manager=true")
	list, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return "1"
	}
	for _, c := range list {
		switch c.State {
		case container.StateExited, container.StateCreated, container.StateDead:
			continue
		}
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
		if status.StatusCode == toolsWaitExit {
			m.toolsWaiting.Store(true)
			log.Printf("Shared tools volume %s: a newer tools image waits for the running desktops to stop; it is applied when a desktop is next created with none running, or at the next start with none running.", ToolsVolumeName)
			return nil
		}
		if status.StatusCode != 0 {
			return fmt.Errorf("tools init container exited with status %d", status.StatusCode)
		}
	}

	m.toolsWaiting.Store(false)
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
