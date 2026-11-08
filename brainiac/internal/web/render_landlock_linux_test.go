//go:build linux

package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"brainiac/internal/isolate"
)

// Live on a Linux kernel with Landlock (N-W4-8): a child started the way
// Render starts the browser, under browserPolicy, may write its profile and
// read the system, and is refused a file outside the allowed set (EACCES),
// another process's environment, and writing anywhere but its profile.
func TestLandlockKeepsTheBrowserInItsFolders(t *testing.T) {
	if !isolate.CanSandbox() {
		t.Skip("this kernel offers no Landlock; the browser runs without a file policy here")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("not for the browser"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, landlockProbeArg,
		"w:"+filepath.Join(profile, "cookies"), // its profile: allowed
		"/usr/lib/os-release",                  // the system read set: allowed where it exists
		secret,                                 // the operator's file: refused
		"/proc/1/environ",                      // another process's secrets: refused
		"w:"+filepath.Join(outside, "dropped"), // writing outside the profile: refused
	)
	cmd.Env = isolate.Env()
	if err := confineBrowser(cmd, browserPolicy(self, profile)); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := string(out)
	t.Logf("under the policy:\n%s", got)
	for _, want := range []string{"ok w:" + filepath.Join(profile, "cookies"), "denied " + secret, "denied /proc/1/environ",
		"denied w:" + filepath.Join(outside, "dropped")} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("want %q", want)
		}
	}
	if _, err := os.Stat("/usr/lib/os-release"); err == nil && !strings.Contains(got, "ok /usr/lib/os-release\n") {
		t.Errorf("the system read set was refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "dropped")); !os.IsNotExist(err) {
		t.Errorf("a file was written outside the profile: %v", err)
	}
}
