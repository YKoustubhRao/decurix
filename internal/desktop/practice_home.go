package desktop

import (
	"context"
	"fmt"
	"io"
)

// PracticeHome is the as-worker file access of practice desktops, by container id, in the shape the run puller reads
// (runs.Home): an eval case's bundle is pulled off its practice desktop before the next case resets it (O-6). Any
// container but a practice desktop (a worker's desktop, a mock app) is refused before anything is read or written.
type PracticeHome struct{ M *Manager }

// practiceOnly refuses a container that is not a practice desktop.
func (h PracticeHome) practiceOnly(ctx context.Context, id string) error {
	inspect, err := h.M.client.ContainerInspect(ctx, id)
	if err != nil {
		if nf := notFound(id, err); nf != err {
			return nf
		}
		return fmt.Errorf("inspect %s: %w", id, err)
	}
	if inspect.Config == nil || inspect.Config.Labels[PracticeLabel] != "true" || inspect.Config.Labels["decurix-practice-mock"] == "true" {
		return ErrNotPractice
	}
	return nil
}

// ReadTar is Manager.ReadTar for practice desktop id.
func (h PracticeHome) ReadTar(ctx context.Context, id, p string, limit int64) (io.ReadCloser, error) {
	clean, err := homePath(p, false)
	if err != nil {
		return nil, fmt.Errorf("read from practice desktop: %w", err)
	}
	if limit <= 0 {
		limit = defaultHomeReadLimit
	}
	if err := h.practiceOnly(ctx, id); err != nil {
		return nil, err
	}
	target, done, err := h.M.homeAccessOf(ctx, id, id)
	if err != nil {
		return nil, err
	}
	rc, err := h.M.readHomeTar(ctx, target, clean, limit, done)
	if err != nil {
		done()
		return nil, err
	}
	return rc, nil
}

// WriteTar is Manager.WriteTar for practice desktop id (the puller writes EXPORTED back).
func (h PracticeHome) WriteTar(ctx context.Context, id, dir string, archive io.Reader) error {
	clean, err := homePath(dir, true)
	if err != nil {
		return fmt.Errorf("write into practice desktop: %w", err)
	}
	if err := h.practiceOnly(ctx, id); err != nil {
		return err
	}
	target, done, err := h.M.homeAccessOf(ctx, id, id)
	if err != nil {
		return err
	}
	defer done()
	return h.M.execTarIn(ctx, target, clean, archive, "--no-overwrite-dir")
}

// PersistMemoryFile is Manager.PersistMemoryFile for practice desktop id: what the worker keeps in its memory reaches
// its next case, as it would on its own desktop. Found in the learning curves (2026-11-16): the broker dropped every
// lesson, so each case asked the expert the same questions again.
func (h PracticeHome) PersistMemoryFile(ctx context.Context, id, filename, content string) error {
	buf, err := memoryTar(filename, content)
	if err != nil {
		return err
	}
	if err := h.practiceOnly(ctx, id); err != nil {
		return err
	}
	target, done, err := h.M.homeAccessOf(ctx, id, id)
	if err != nil {
		return err
	}
	defer done()
	return h.M.persistMemoryTar(ctx, target, id, buf)
}
