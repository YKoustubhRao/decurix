package desktop

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

//go:embed windows-app-icon.ps1
var windowsAppIconScript string

// Copy pixels, never an exe/lnk or a Windows-only theme name, into Linux.
func windowsAppIcon(app HostAppItem) []byte {
	if app.AppType != "windows_app" {
		return nil
	}
	target, _, ok := windowsTarget(app.HostCmd)
	if !ok {
		return nil
	}
	request, _ := json.Marshal(map[string]string{"target": target, "name": app.Name})
	script := "& {\n" + windowsAppIconScript + "\n} -Request '" + base64.StdEncoding.EncodeToString(request) + "'"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	// A Windows executable on /mnt/c is visible in a Linux container but
	// cannot run there. Cross to WSL's host namespace before invoking it.
	if _, err := os.Stat("/var/run/docker.sock"); err == nil && findDockerBinary() != "" {
		hostScript := "PATH=$PATH:/mnt/c/Windows/System32/WindowsPowerShell/v1.0:/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0 powershell.exe -NoProfile -NonInteractive -Command " + shellQuote(script)
		cmd = HopCommand(ctx, findDockerBinary(), "app-icon", false, hostScript)
	} else if ps := findPowershellExe(); ps != "" {
		cmd = exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command", script)
		HideWindow(cmd)
	} else {
		return nil
	}
	cmd.Stdin = strings.NewReader("")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
	if err != nil || len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil
	}
	return png
}
