package decurix

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// toolsCmd is the tools image's populate rule, with its two folders moved to src and dst.
func toolsCmd(t *testing.T, src, dst string) string {
	t.Helper()
	raw, err := os.ReadFile("docker/tools.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	cmds := regexp.MustCompile(`(?m)^CMD (\[.*\])\s*$`).FindAllStringSubmatch(string(raw), -1)
	var argv []string
	if len(cmds) == 0 || json.Unmarshal([]byte(cmds[len(cmds)-1][1]), &argv) != nil || len(argv) != 3 {
		t.Fatal("no shell CMD in docker/tools.Dockerfile")
	}
	return strings.NewReplacer("/opt/tools", src, "/target", dst).Replace(argv[2])
}

// The tools volume's ready marker is written last, after everything else is in place; a refresh waits while desktops
// run; a matching marker leaves the volume alone (RW1-D-02).
func TestTheToolsVolumeIsReadyOnlyWhenWhole(t *testing.T) {
	cmd := toolsCmd(t, "/S", "/D")
	if strings.LastIndex(cmd, "mv /D/$R.new /D/$R") < strings.LastIndex(cmd, "cp -a") {
		t.Fatal("the ready marker is not written after the copy")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the populate rule runs in a Linux container; run it in WSL")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	os.MkdirAll(filepath.Join(src, "go", "bin"), 0o755)
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(src, "go", "bin", "go"), []byte("new"), 0o755)
	os.WriteFile(filepath.Join(src, ".decurix-tools-ready"), []byte("v2"), 0o644)
	os.WriteFile(filepath.Join(dst, ".decurix-tools-ready"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(dst, "stale"), []byte("x"), 0o644)
	run := func(wantExit int, env ...string) {
		c := exec.Command("sh", "-c", toolsCmd(t, src, dst))
		c.Env = append(os.Environ(), env...)
		out, err := c.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if code != wantExit {
			t.Fatalf("exit %d, want %d: %s", code, wantExit, out)
		}
	}
	// While desktops run, the refresh waits and says so with exit 75 (desktop.toolsWaitExit), leaving the volume as is.
	run(75, "DECURIX_DESKTOPS_RUNNING=1")
	if b, _ := os.ReadFile(filepath.Join(dst, ".decurix-tools-ready")); string(b) != "v1" {
		t.Fatal("a refresh ran while desktops were running")
	}
	run(0, "DECURIX_DESKTOPS_RUNNING=0")
	if b, _ := os.ReadFile(filepath.Join(dst, ".decurix-tools-ready")); string(b) != "v2" {
		t.Fatal("the marker was not written")
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); err == nil {
		t.Fatal("the old tree was left")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "go", "bin", "go")); string(b) != "new" {
		t.Fatal("the tools were not copied")
	}
}
