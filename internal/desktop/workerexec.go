package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// workerExecPath is the PATH of a command the control plane runs in a desktop as the worker.
const workerExecPath = "/opt/tools/bin:/opt/tools/node/bin:/usr/local/bin:/usr/bin:/bin"

// workerCmd wraps cmd so it runs with an empty environment plus HOME, USER, LOGNAME and PATH. A `docker exec` gets the
// container's whole environment by default; nothing the control plane runs in a desktop needs more than this, and a
// command the worker can influence must not see what the environment holds (round-3 recheck).
func workerCmd(cmd ...string) []string {
	return append([]string{"/usr/bin/env", "-i", "HOME=/home/headless", "USER=headless", "LOGNAME=headless",
		"PATH=" + workerExecPath, "LANG=C.UTF-8"}, cmd...)
}

// gateKeyArchive is a tar to extract at "/" that holds key as GateKeyFile: root-owned, 0400, in a 0700 directory.
func gateKeyArchive(key string) io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := strings.TrimPrefix(path.Dir(GateKeyFile), "/") + "/"
	_ = tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o700, ModTime: time.Unix(0, 0)})
	_ = tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(GateKeyFile, "/"), Typeflag: tar.TypeReg, Mode: 0o400,
		Size: int64(len(key)), ModTime: time.Unix(0, 0)})
	_, _ = tw.Write([]byte(key))
	_ = tw.Close()
	return &buf
}

// putGateKey puts a created (not yet started) desktop's approval key in place for its entrypoint.
func (m *Manager) putGateKey(ctx context.Context, containerID, key string) error {
	if key == "" {
		return errors.New("the control plane has no secret to derive the key from")
	}
	// Root-side on purpose, the one copy that must be: a root-only file outside the home, into a container that has
	// never started, so nothing on the path is the worker's.
	return m.client.CopyToContainer(ctx, containerID, "/", gateKeyArchive(key), container.CopyToContainerOptions{CopyUIDGID: true})
}

// ErrNoGate is returned for an image that has no approval gate (no GateUser): an image from before the gate, or one
// built without it. Its desktops would keep approvals where the worker can write them, so none is made.
var ErrNoGate = errors.New("this desktop image has no approval gate; rebuild the image")

// prepareGate checks a created (not yet started) desktop's image has an approval gate and puts its key in place.
func (m *Manager) prepareGate(ctx context.Context, containerID, key string) error {
	passwd, err := m.copyOut(ctx, containerID, "/etc/passwd", 1<<20)
	if err != nil {
		return fmt.Errorf("read the image's users: %w", err)
	}
	if !tarHasUser(passwd, GateUser) {
		return ErrNoGate
	}
	return m.putGateKey(ctx, containerID, key)
}

// tarHasUser reports whether the passwd file in a tar (as CopyFromContainer gives it) has a line for user.
func tarHasUser(archive []byte, user string) bool {
	tr := tar.NewReader(bytes.NewReader(archive))
	h, err := tr.Next()
	if err != nil || h.Typeflag != tar.TypeReg {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(tr, 1<<20))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, user+":") {
			return true
		}
	}
	return false
}
