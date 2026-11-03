package desktop

import (
	"archive/tar"
	"bytes"
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

// passwdTar is /etc/passwd as CopyFromContainer gives it.
func passwdTar(body string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	return buf.Bytes()
}

// TestAnImageWithoutTheGateUserHasNoGate checks only an image whose passwd names the gate's user counts as having one
// (round-4 recheck: desktops from older images fell back to a store the worker can write).
func TestAnImageWithoutTheGateUserHasNoGate(t *testing.T) {
	with := "root:x:0:0::/root:/bin/bash\nheadless:x:1001:1001::/home/headless:/bin/bash\ndecurixgate:x:999:999::/nonexistent:/usr/sbin/nologin\n"
	without := "root:x:0:0::/root:/bin/bash\nheadless:x:1001:1001::/home/headless:/bin/bash\n"
	if !tarHasUser(passwdTar(with), GateUser) {
		t.Fatal("the gate's user was not found")
	}
	for name, archive := range map[string][]byte{
		"no gate user":                    passwdTar(without),
		"a name that only starts like it": passwdTar("decurixgatex:x:5:5::/:/bin/sh\n"),
		"not a tar":                       []byte("decurixgate:x:999:999::/:/bin/sh\n"),
	} {
		if tarHasUser(archive, GateUser) {
			t.Fatalf("%s: taken for an image with a gate", name)
		}
	}
}

// TestEveryDesktopIsToldItMustHaveAGate checks the control plane's flag is in a new desktop's environment.
func TestEveryDesktopIsToldItMustHaveAGate(t *testing.T) {
	for _, kv := range buildDesktopEnv("d1", "", "", "", Brain{}) {
		if kv == GateRequiredEnv+"=1" {
			return
		}
	}
	t.Fatal("no " + GateRequiredEnv + " in a new desktop's environment")
}
