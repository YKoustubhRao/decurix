package desktop

import (
	"archive/tar"
	"io"
	"strings"
	"testing"
)

// TestGateKeyArchiveIsRootOnly checks the approval key goes in as a root-owned 0400 file in a 0700 directory.
func TestGateKeyArchiveIsRootOnly(t *testing.T) {
	tr := tar.NewReader(gateKeyArchive("k3y"))
	seen := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[h.Name] = h
		if h.Typeflag == tar.TypeReg {
			body, _ := io.ReadAll(tr)
			if string(body) != "k3y" {
				t.Fatalf("key body %q", body)
			}
		}
	}
	dir, file := seen["etc/decurix-gate/"], seen["etc/decurix-gate/key"]
	if dir == nil || file == nil {
		t.Fatalf("entries: %v", seen)
	}
	if dir.Mode != 0o700 || file.Mode != 0o400 || dir.Uid != 0 || file.Uid != 0 || file.Gid != 0 {
		t.Fatalf("dir %o uid %d, file %o uid %d gid %d", dir.Mode, dir.Uid, file.Mode, file.Uid, file.Gid)
	}
}

// TestWorkerCmdStartsFromAnEmptyEnvironment checks every exec the control plane runs in a desktop drops the
// container's environment and never puts the worker's own bin folder on PATH.
func TestWorkerCmdStartsFromAnEmptyEnvironment(t *testing.T) {
	cmd := workerCmd("tar", "-x")
	if cmd[0] != "/usr/bin/env" || cmd[1] != "-i" {
		t.Fatalf("not env -i: %v", cmd)
	}
	if strings.Contains(strings.Join(cmd, " "), "/home/headless/.local") {
		t.Fatalf("worker bin on PATH: %v", cmd)
	}
	if cmd[len(cmd)-2] != "tar" || cmd[len(cmd)-1] != "-x" {
		t.Fatalf("command lost: %v", cmd)
	}
}

// TestNewDesktopEnvHoldsNoGateKey checks the approval key is never in a desktop's environment.
func TestNewDesktopEnvHoldsNoGateKey(t *testing.T) {
	for _, kv := range buildDesktopEnv("d1", "", "", "", Brain{}) {
		if strings.Contains(kv, "GATE_KEY") {
			t.Fatalf("gate key in env: %s", kv)
		}
	}
}
