package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// The host-app helper's processes on the person's own computer are ended by their own ids, and only after checking
// they are still what was started: a helper is a PowerShell running decurix-winhost\bridge.ps1, and the app it
// launched is that helper's child. A process id the host has since given to something else - the person's own Word
// included - is left alone. Found live (2026-10-20): /api/host-gui/stop answered 204 and the helper and the
// WINWORD.EXE it had started kept running; a Windows program started through WSL outlives the Linux side that started
// it, and nothing else ended them.

// helperMatch is how a host-app helper's command line reads (writeHostGUIScript's folder, the script's name).
const helperMatch = `*decurix-winhost*bridge.ps1*`

// reapScript is the PowerShell that ends helper helperPID's process tree (if it is still a helper) and app appPID's
// tree (if it is still that helper's child). 0 skips either.
func reapScript(helperPID, appPID int) string {
	var b strings.Builder
	b.WriteString(quietPreamble)
	if appPID > 0 {
		fmt.Fprintf(&b, "$a = Get-CimInstance Win32_Process -Filter 'ProcessId=%d'\n", appPID)
		fmt.Fprintf(&b, "if ($a -and $a.ParentProcessId -eq %d) { taskkill.exe /T /F /PID %d | Out-Null }\n", helperPID, appPID)
	}
	if helperPID > 0 {
		fmt.Fprintf(&b, "$h = Get-CimInstance Win32_Process -Filter 'ProcessId=%d'\n", helperPID)
		fmt.Fprintf(&b, "if ($h -and $h.CommandLine -like '%s') { taskkill.exe /T /F /PID %d | Out-Null }\n", helperMatch, helperPID)
	}
	return b.String()
}

// sweepScript is the PowerShell that ends the process tree of every host-app helper except those in keep: at the
// control plane's start (keep is empty - a helper's owner was an earlier run, now gone) the helpers left by a crash.
func sweepScript(keep []int) string {
	ids := make([]string, 0, len(keep))
	for _, id := range keep {
		ids = append(ids, strconv.Itoa(id))
	}
	return quietPreamble +
		"$keep = @(" + strings.Join(ids, ",") + ")\n" +
		"Get-CimInstance Win32_Process -Filter \"Name='powershell.exe'\" | " +
		"Where-Object { $_.CommandLine -like '" + helperMatch + "' -and $keep -notcontains [int]$_.ProcessId } | " +
		"ForEach-Object { taskkill.exe /T /F /PID $_.ProcessId | Out-Null; 'ENDED ' + $_.ProcessId }\n"
}

// quietPreamble keeps PowerShell's own chatter (errors of a process already gone, the "Preparing modules for first use"
// progress records it writes as CLIXML) out of what a script says: only its own lines are read back.
const quietPreamble = "$ErrorActionPreference = 'SilentlyContinue'\n$ProgressPreference = 'SilentlyContinue'\n"

// encodedCommand is a script as PowerShell's -EncodedCommand takes it (UTF-16LE, base64): no quoting to get wrong.
func encodedCommand(script string) string {
	u := utf16.Encode([]rune(script))
	buf := make([]byte, 2*len(u))
	for i, c := range u {
		buf[2*i], buf[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// runOnHost runs a PowerShell script on the Windows host and returns what it printed: directly for a control plane
// that is a Windows program, through WSL otherwise (a host hop when the control plane is in a container).
func runOnHost(script string, timeout time.Duration) (string, error) {
	if !hostGUISupported() {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		ps := desktop.PowershellExe()
		if ps == "" {
			return "", fmt.Errorf("Windows PowerShell was not found")
		}
		cmd = exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedCommand(script))
		desktop.HideWindow(cmd)
	} else {
		shell := `PATH="$PATH:/mnt/c/Windows/System32:/mnt/c/Windows/System32/WindowsPowerShell/v1.0" ` +
			`powershell.exe -NoProfile -NonInteractive -EncodedCommand ` + encodedCommand(script)
		cmd = exec.CommandContext(ctx, "bash", "-c", shell)
		if _, err := exec.LookPath("powershell.exe"); err != nil {
			if _, statErr := os.Stat("/var/run/docker.sock"); statErr == nil {
				if dockerBin := findDockerBinary(); dockerBin != "" {
					cmd = desktop.HopCommand(ctx, dockerBin, "reap", false, shell)
				}
			}
		}
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr // progress records and the like: never mixed into what the script printed
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), err
}

// reapHostProcesses is winhost.Manager.Reap: what a stopped helper left on the host, ended by its own ids.
func reapHostProcesses(helperPID, appPID int) {
	if helperPID <= 0 && appPID <= 0 {
		return
	}
	if out, err := runOnHost(reapScript(helperPID, appPID), 30*time.Second); err != nil {
		log.Printf("host apps: could not end the helper %d / app %d on the host: %v %s", helperPID, appPID, err,
			strings.TrimSpace(out))
	}
}

// SweepHostHelpersAtStart ends the host-app helpers (and the apps they started) an earlier run of the control plane
// left on the host: called once at start, before any app is started or kept, so every helper there is an orphan.
func SweepHostHelpersAtStart() {
	if !hostGUISupported() {
		return
	}
	out, err := runOnHost(sweepScript(nil), 60*time.Second)
	if err != nil {
		log.Printf("host apps: could not look for helpers an earlier run left on the host: %v", err)
		return
	}
	if ended := endedPIDs(out); len(ended) > 0 {
		log.Printf("host apps: ended %d helper(s) an earlier run left on the host (and the apps they started): %s",
			len(ended), strings.Join(ended, ", "))
	}
}

// endedPIDs reads the sweep's own "ENDED <pid>" lines, and nothing else it may have printed.
func endedPIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "ENDED" {
			if _, err := strconv.Atoi(f[1]); err == nil {
				ids = append(ids, f[1])
			}
		}
	}
	return ids
}
