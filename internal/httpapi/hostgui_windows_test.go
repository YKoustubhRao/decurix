//go:build windows

package httpapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// On Windows itself the helper is Windows PowerShell run directly on a script in the temp folder - no shell, no wslpath,
// no docker hop. This runs the real bridge.ps1 and asks it a question, without starting any app.
func TestNativeWindowsHelperAnswersAProtocolCheck(t *testing.T) {
	if !hostGUISupported() {
		t.Fatal("a Windows control plane must support host apps")
	}
	m := winhost.NewManager(hostGUIStarter)
	t.Cleanup(m.StopAll)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := winhost.Target{Target: `C:\Windows\System32\notepad.exe`}
	got, err := m.Check(ctx, []winhost.Target{target})
	if err != nil {
		t.Fatalf("the native helper did not answer: %v", err)
	}
	if _, ok := got[target]; !ok {
		t.Fatalf("the helper gave no verdict for %v: %v", target, got)
	}
}

// createHostCmd runs the program itself on Windows: no bash around it, and so no second layer of quoting.
func TestCreateHostCmdRunsProgramsDirectlyOnWindows(t *testing.T) {
	cmd := createHostCmd(nil, "", "cmd.exe", "/c", "echo", "hello")
	if filepath.Base(cmd.Path) != "cmd.exe" {
		t.Fatalf("ran %q, want cmd.exe itself and not a shell around it", cmd.Path)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "hello") {
		t.Fatalf("output = %q, want it to contain hello", out)
	}
}
