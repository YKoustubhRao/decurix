package desktop

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

//go:embed windows-app-icon.ps1
var windowsAppIconScript string

// Copy pixels, never an exe/lnk or a Windows-only theme name, into Linux.
func windowsAppIcon(app HostAppItem) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), windowsAppIconTimeout)
	defer cancel()
	png, _ := extractWindowsAppIcon(ctx, app)
	return png
}

// windowsAppIconTimeout bounds one icon extraction while a desktop's launchers are packed: past it the launcher gets
// its drawn icon instead, so a slow or stuck PowerShell never holds up the creation.
const windowsAppIconTimeout = 20 * time.Second

// errNoWindowsIcon says the extraction ran and gave no PNG: not a Windows app, no target, no icon in it, or no shell
// to ask.
var errNoWindowsIcon = errors.New("no icon")

// extractWindowsAppIcon asks the Windows shell for the app's icon as PNG bytes, within ctx, and says why when it gives
// none.
func extractWindowsAppIcon(ctx context.Context, app HostAppItem) ([]byte, error) {
	if app.AppType != "windows_app" {
		return nil, errNoWindowsIcon
	}
	target, _, ok := windowsTarget(app.HostCmd)
	if !ok {
		return nil, errNoWindowsIcon
	}
	request, _ := json.Marshal(map[string]string{"target": target, "name": app.Name})
	script := "& {\n" + windowsAppIconScript + "\n} -Request '" + base64.StdEncoding.EncodeToString(request) + "'"
	var cmd *exec.Cmd
	// A Windows executable on /mnt/c is visible in a Linux container but
	// cannot run there. Cross to WSL's host namespace before invoking it.
	if _, err := os.Stat("/var/run/docker.sock"); err == nil && findDockerBinary() != "" {
		hostScript := "PATH=$PATH:/mnt/c/Windows/System32/WindowsPowerShell/v1.0:/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0 powershell.exe -NoProfile -NonInteractive -Command " + shellQuote(script)
		cmd = HopCommand(ctx, findDockerBinary(), "app-icon", false, hostScript)
	} else if ps := PowershellExe(); ps != "" {
		cmd = exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command", script)
		HideWindow(cmd)
	} else {
		return nil, errNoWindowsIcon
	}
	cmd.Stdin = strings.NewReader("")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
	if err != nil || len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, errNoWindowsIcon
	}
	return png, nil
}
