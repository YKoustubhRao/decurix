//go:build windows

package winhost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realBridge is a Manager whose helpers run the real bridge.ps1 in Windows PowerShell, with LOCALAPPDATA - where the
// bridge keeps its compiled code and the apps' private profiles - moved to local.
func realBridge(t *testing.T, local string) *Manager {
	t.Helper()
	script := filepath.Join(t.TempDir(), "bridge.ps1")
	if err := os.WriteFile(script, Script, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(func(ctx context.Context, name string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script,
			"-DesktopName", name)
		cmd.Env = append(os.Environ(), "LOCALAPPDATA="+local)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		return cmd, nil
	})
	t.Cleanup(m.StopAll)
	return m
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// junction makes link a directory junction to target (no administrator needed, unlike a symbolic link).
func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestTheRealBridgeRemovesOnlyTheDeletedVMsProfiles runs dropprofiles in the real bridge.ps1 (B-19): a deleted VM's app
// profiles go, read-only files included; a junction inside one or in place of one is removed as a link and what it
// points at stays; a folder whose name only starts like the VM's, another VM's, and anything outside the profile root
// stay; a file still held open is left and counted, and the next delete removes it.
func TestTheRealBridgeRemovesOnlyTheDeletedVMsProfiles(t *testing.T) {
	oldTries, oldRetry := profileTries, profileRetry
	profileTries, profileRetry = 2, 50*time.Millisecond
	defer func() { profileTries, profileRetry = oldTries, oldRetry }()

	local := t.TempDir()
	root := filepath.Join(local, "Decurix", "hidden-profiles")
	outside := filepath.Join(local, "outside")
	mustWrite(t, filepath.Join(outside, "a", "keep.txt"), "the person's")
	mustWrite(t, filepath.Join(outside, "b", "keep.txt"), "the person's")

	mustWrite(t, filepath.Join(root, "decurix_vm1_a-chrome", "Default", "Cookies"), "sign-in")
	readOnly := filepath.Join(root, "decurix_vm1_a-chrome", "Default", "Preferences")
	mustWrite(t, readOnly, "{}")
	if err := os.Chmod(readOnly, 0o400); err != nil {
		t.Fatal(err)
	}
	junction(t, filepath.Join(root, "decurix_vm1_a-chrome", "Default", "escape"), filepath.Join(outside, "a"))
	mustWrite(t, filepath.Join(root, "decurix_vm1_a-code", "User", "settings.json"), "{}")
	junction(t, filepath.Join(root, "decurix_vm1_b-msedge"), filepath.Join(outside, "b"))
	held := filepath.Join(root, "decurix_vm1_c-chrome", "Default", "Cookies")
	mustWrite(t, held, "held")
	mustWrite(t, filepath.Join(root, "decurix_vm1_c-chrome", "Default", "History"), "h")

	keep := []string{
		filepath.Join(root, "decurix_vm1_a1-chrome", "x"),            // starts like vm1-a's name
		filepath.Join(root, "decurix_vm1_a_b-chrome", "x"),           // likewise
		filepath.Join(root, "decurix_vm2_a-chrome", "x"),             // another VM's
		filepath.Join(root, "decurix_vm1_a", "x"),                    // no app part: not a profile
		filepath.Join(local, "Decurix", "decurix_vm1_a-chrome", "x"), // outside the root
	}
	for _, k := range keep {
		mustWrite(t, k, "keep")
	}
	mustWrite(t, filepath.Join(root, "decurix_vm1_a-notes"), "a file, not a profile folder")
	keep = append(keep, filepath.Join(root, "decurix_vm1_a-notes"))

	p, err := syscall.UTF16PtrFromString(held)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			syscall.CloseHandle(h)
		}
	}()

	m := realBridge(t, local)
	removed, err := m.DeleteDesktop("vm1", []string{"vm1-a", "vm1-b", "vm1-c"})
	if removed != 3 {
		t.Fatalf("removed %d profile folders, want 3 (%v)", removed, err)
	}
	if err == nil || !strings.Contains(err.Error(), "held open") {
		t.Fatalf("err = %v, want the held profile reported", err)
	}
	for _, gone := range []string{"decurix_vm1_a-chrome", "decurix_vm1_a-code", "decurix_vm1_b-msedge"} {
		if exists(filepath.Join(root, gone)) {
			t.Errorf("%s was not removed", gone)
		}
	}
	for _, k := range append(keep, filepath.Join(outside, "a", "keep.txt"), filepath.Join(outside, "b", "keep.txt"), held) {
		if !exists(k) {
			t.Errorf("%s was removed", k)
		}
	}

	syscall.CloseHandle(h)
	closed = true
	if removed, err := m.DeleteDesktop("vm1", []string{"vm1-c"}); removed != 1 || err != nil {
		t.Fatalf("after the file was let go: removed %d, %v", removed, err)
	}
	if exists(filepath.Join(root, "decurix_vm1_c-chrome")) {
		t.Error("the profile that was held open is still there once it was let go")
	}

	// The bridge checks the names itself, whatever it is sent.
	for _, bad := range []string{"..", `decurix_vm2_a-chrome`, `decurix_..\x`, "other_vm1", ""} {
		if _, _, _, err := m.dropProfiles("dropprofiles\t" + bad); err == nil || !strings.Contains(err.Error(), "invalid desktop name") {
			t.Errorf("dropprofiles %q: %v, want refused", bad, err)
		}
	}
	for _, k := range keep {
		if !exists(k) {
			t.Errorf("%s was removed", k)
		}
	}
}
