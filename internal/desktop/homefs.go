package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
)

// The desktop's home, read and written as the worker.
//
// Everything under /home/headless belongs to the worker (uid 1001), who can shape it: a folder there can be a link
// to anywhere in the container. Docker's copy API (CopyFromContainer / CopyToContainer) runs in the daemon as root and
// resolves a path's parent folders through such links, so a root-side copy "into ~/inbox/x" could read or write
// root-only places in the desktop - the approval gate's key among them (B-01). Every copy into or out of a desktop's
// home therefore goes through this file layer, which runs tar inside the container as 1001:1001 with an empty
// environment: whatever a link points at, tar can only reach what the worker could reach anyway.
//
//   - a running desktop: tar is exec'd in it; a read does NOT stop the worker's processes (execTarOut's freeze is for
//     offline containers only - on a live desktop it froze the worker for good, N-B3);
//   - a paused one: unpaused for the copy and paused again after it;
//   - a stopped (or not yet started) one: a throwaway container from the same image with only the desktop's home
//     volume mounted, user 1001, no network, no capabilities, no-new-privileges and a read-only root, removed after.
//
// The control plane's root-side copies that remain each touch a fixed root-owned path or a container that has never
// run, and say so where they are (prepareGate, the passwd read, the practice and login containers, missingapps).

// homeDir is the worker's home in every desktop.
const homeDir = "/home/headless"

// defaultHomeReadLimit bounds a read of a desktop's home when the caller names no limit.
const defaultHomeReadLimit = 1 << 30

// homeHelperLabel marks a throwaway container this layer made, so one a crash left behind can be found and removed.
const homeHelperLabel = "decurix.homefs"

// errHomePath is a path outside the worker's home.
var errHomePath = errors.New("not a path under " + homeDir)

// homePath cleans p and checks it is under the home (the home itself only when allowHome).
func homePath(p string, allowHome bool) (string, error) {
	clean := path.Clean(p)
	if clean == homeDir && allowHome {
		return clean, nil
	}
	if !strings.HasPrefix(clean+"/", homeDir+"/") || clean == homeDir {
		return "", fmt.Errorf("%q: %w", p, errHomePath)
	}
	return clean, nil
}

// ReadTar reads one file or folder under the home of desktop id, as the worker, as a tar stream whose entries are
// named from the path's own name down (as `docker cp` names them). A path that is not there is ErrNotFound. The
// stream ends with an error if it passes limit bytes (0: defaultHomeReadLimit) or if tar failed; the caller closes it.
func (m *Manager) ReadTar(ctx context.Context, id, p string, limit int64) (io.ReadCloser, error) {
	clean, err := homePath(p, false)
	if err != nil {
		return nil, fmt.Errorf("read from desktop: %w", err)
	}
	if limit <= 0 {
		limit = defaultHomeReadLimit
	}
	target, done, err := m.homeAccess(ctx, id)
	if err != nil {
		return nil, err
	}
	rc, err := m.readHomeTar(ctx, target, clean, limit, done)
	if err != nil {
		done()
		return nil, err
	}
	return rc, nil
}

// WriteTar extracts archive (entries relative to dir) into dir under the home of desktop id, as the worker, creating
// nothing the worker could not. dir may be the home itself. It works on a stopped desktop too, which is what lets an
// email's attachments be in place before the request that names them wakes the desktop up.
func (m *Manager) WriteTar(ctx context.Context, id, dir string, archive io.Reader) error {
	clean, err := homePath(dir, true)
	if err != nil {
		return fmt.Errorf("write into desktop: %w", err)
	}
	target, done, err := m.homeAccess(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	// An existing folder keeps its own owner and mode: the archive's say-so is for folders it creates.
	return m.execTarIn(ctx, target, clean, archive, "--no-overwrite-dir")
}

// homeAccess makes desktop id's home reachable by exec: it returns the container to exec in and what to do once the
// copy is over (re-pause, or remove the throwaway container).
func (m *Manager) homeAccess(ctx context.Context, id string) (string, func(), error) {
	return m.homeAccessOf(ctx, "desktop_"+sanitizeName(id), id)
}

// homeAccessOf is homeAccess for the container called name (a practice desktop's has its own name); id names the
// desktop in a not-found error.
func (m *Manager) homeAccessOf(ctx context.Context, name, id string) (string, func(), error) {
	// One copy at a time decides how to reach a desktop: two that both read "paused" raced on the unpause, and the
	// loser failed its copy instead of joining the first (REVIEW-3 follow-up 3).
	defer homeCopyPauses.lockName(name)()
	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil {
		if nf := notFound(id, err); nf != err {
			return "", nil, nf
		}
		return "", nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	state := inspect.State
	repause := func() {
		// Under the same lock, so a copy starting now either joins before this one ends or sees the desktop paused.
		defer homeCopyPauses.lockName(name)()
		if !homeCopyPauses.end(inspect.ID) {
			return
		}
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := m.client.ContainerPause(pctx, inspect.ID); err != nil {
			log.Printf("desktop: pausing %s again after a copy: %v", name, err)
		}
		m.forgetShotTargetOf(inspect.ID) // a frozen screen: the thumbnail asks Docker next time
	}
	switch {
	case state != nil && state.Running && state.Paused:
		if err := m.client.ContainerUnpause(ctx, inspect.ID); err != nil {
			// Resumed meanwhile from outside the copies (a person's Resume): read it again, and copy into it as into
			// any running desktop. Still paused, or gone: the copy fails.
			again, ierr := m.client.ContainerInspect(ctx, name)
			if ierr != nil || again.State == nil || !again.State.Running || again.State.Paused {
				return "", nil, fmt.Errorf("resume %s for a copy: %w", name, err)
			}
			if homeCopyPauses.joining(inspect.ID) {
				return inspect.ID, repause, nil
			}
			return inspect.ID, func() {}, nil
		}
		homeCopyPauses.begin(inspect.ID, true)
		return inspect.ID, repause, nil
	case state != nil && state.Running:
		// Running because another copy resumed it: this copy counts too, so the first to finish does not pause the
		// desktop under the second (RW1-C-04).
		if homeCopyPauses.joining(inspect.ID) {
			return inspect.ID, repause, nil
		}
		return inspect.ID, func() {}, nil
	}
	return m.homeHelper(ctx, name, inspect)
}

// homeHelper starts the throwaway container that reaches a stopped desktop's home: the desktop's own image (so the
// same tar and the same uid), its home volume and nothing else of it.
func (m *Manager) homeHelper(ctx context.Context, name string, inspect container.InspectResponse) (string, func(), error) {
	volume := ""
	for _, mp := range inspect.Mounts {
		if mp.Destination == homeDir && mp.Type == mount.TypeVolume {
			volume = mp.Name
		}
	}
	if volume == "" {
		return "", nil, fmt.Errorf("%s has no home volume to copy through", name)
	}
	image := inspect.Image
	if inspect.Config != nil && inspect.Config.Image != "" && image == "" {
		image = inspect.Config.Image
	}
	cfg, host := homeHelperConfigs(image, volume, name)
	created, err := m.client.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", nil, fmt.Errorf("make a helper to reach %s's home: %w", name, err)
	}
	remove := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := m.client.ContainerRemove(rctx, created.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Printf("desktop: removing the home helper of %s: %v", name, err)
		}
	}
	if err := m.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		remove()
		return "", nil, fmt.Errorf("start a helper to reach %s's home: %w", name, err)
	}
	return created.ID, remove, nil
}

// homeHelperConfigs is the throwaway container's configuration: the worker's uid, an empty environment, only the home
// volume, no network, no capabilities, no new privileges and a read-only root. It sleeps until removed; the copy is
// an exec in it.
func homeHelperConfigs(image, volume, desktopContainer string) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      image,
		User:       fmt.Sprintf("%d:%d", desktopUID, desktopGID),
		Env:        []string{},
		Entrypoint: []string{"/usr/bin/env", "-i", "/bin/sleep", "3600"},
		Cmd:        []string{},
		WorkingDir: "/",
		// Found and removed by its desktop's delete and by this instance's start-up sweep (RW1-C-05).
		Labels: map[string]string{homeHelperLabel: desktopContainer, homeHelperInstanceLabel: hopInstance(), "decurix": "true"},
	}
	host := &container.HostConfig{
		NetworkMode:    "none",
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		ReadonlyRootfs: true,
		Mounts:         []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: homeDir}},
	}
	return cfg, host
}

// ErrPathNotFound is a path not there in a desktop that is: errors.Is(err, ErrNotFound) holds for it too, so every
// caller that only asks "not found" is unchanged, and one that must tell the two apart (the run puller) can.
var ErrPathNotFound = fmt.Errorf("path not there: %w", ErrNotFound)

// readHomeTar streams one path out of container target as the worker, without stopping anything there. The first
// exec settles whether the path is there (ErrNotFound) before any byte is returned; the second streams tar. done runs
// once the stream has ended or been closed.
func (m *Manager) readHomeTar(ctx context.Context, target, clean string, limit int64, done func()) (io.ReadCloser, error) {
	// Is it there? As the worker: a link the worker made resolves only as far as the worker may go.
	if code, err := m.execWorker(ctx, target, nil, "/bin/sh", "-c", `[ -e "$1" ]`, "sh", clean); err != nil {
		return nil, err
	} else if code != 0 {
		return nil, ErrPathNotFound
	}
	cmd := homeCmd("tar", "-c", "-C", path.Dir(clean), "--", path.Base(clean))
	exec, err := m.client.ContainerExecCreate(ctx, target, container.ExecOptions{
		User: fmt.Sprintf("%d:%d", desktopUID, desktopGID), Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, err
	}
	attach, err := m.client.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		defer done()
		defer attach.Close()
		out := &limitWriter{w: pw, left: limit}
		// tar's warnings are kept to their first 4 KiB and the rest dropped without an error: only the stdout limit
		// (limitWriter) may stop the copy. A full stderr sink used to end the whole stream there, and a cut archive
		// was then taken as whole (review RW1-C-01).
		stderr := &headBuffer{limit: 4 << 10}
		_, err := stdcopy.StdCopy(out, stderr, attach.Reader)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if err == nil {
			state, ierr := m.client.ContainerExecInspect(context.WithoutCancel(ctx), exec.ID)
			switch {
			case ierr != nil:
				err = ierr
			case state.Running:
				err = errors.New("reading the desktop's files did not finish")
			case state.ExitCode != 0:
				log.Printf("desktop: tar out of %s:%s exited %d %s", shortID(target), clean, state.ExitCode, stderr.String())
				err = errors.New("reading the desktop's files failed")
			}
		}
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// execWorker runs cmd as the worker in container target with an empty environment, feeding it stdin (nil: none), and
// returns its exit status.
func (m *Manager) execWorker(ctx context.Context, target string, stdin io.Reader, cmd ...string) (int, error) {
	exec, err := m.client.ContainerExecCreate(ctx, target, container.ExecOptions{
		User: fmt.Sprintf("%d:%d", desktopUID, desktopGID), Cmd: homeCmd(cmd...),
		AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return 0, err
	}
	attach, err := m.client.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, err
	}
	defer attach.Close()
	if stdin != nil {
		if _, err := io.Copy(attach.Conn, stdin); err != nil {
			return 0, err
		}
		if err := attach.CloseWrite(); err != nil {
			return 0, err
		}
	}
	if _, err := stdcopy.StdCopy(io.Discard, io.Discard, io.LimitReader(attach.Reader, 64<<10)); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	state, err := m.client.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return 0, err
	}
	if state.Running {
		return 0, errors.New("the command in the desktop did not finish")
	}
	return state.ExitCode, nil
}

// homeCmd is cmd run with an empty environment and the system PATH only: the copy's own tar and sh, never a program
// the worker put on its PATH.
func homeCmd(cmd ...string) []string {
	return append([]string{"/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}, cmd...)
}

// limitWriter passes at most left bytes on to w, and stops the copy with errCapped when more arrive: the reader then
// gets that error, never a cut archive.
type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, errCapped
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

// readHomeBytes is ReadTar read whole: the tar of one path under desktop id's home, as the worker, within limit.
func (m *Manager) readHomeBytes(ctx context.Context, id, p string, limit int64) ([]byte, error) {
	rc, err := m.ReadTar(ctx, id, p, limit)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
