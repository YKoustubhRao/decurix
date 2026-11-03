package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
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
	return m.client.CopyToContainer(ctx, containerID, "/", gateKeyArchive(key), container.CopyToContainerOptions{CopyUIDGID: true})
}
