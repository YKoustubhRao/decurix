package winhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The helper can run two ways. Hosted, Windows PowerShell runs bridge.ps1 for as long as the desktop lives: it needs
// nothing prepared, and it is 58 MB of memory per desktop for an interpreter that, once the helper's own code is
// loaded, interprets nothing. Or as a program: the same code, compiled once by that script into
// %LOCALAPPDATA%\Decurix\bridge and started directly, about a third the size and with nothing between a pointer move
// and the code that handles it. A worker's desktop uses the program whenever there is one that has been seen to run,
// and the hosted helper otherwise - the first start on a computer, or one where the program cannot be built or is
// not allowed to run.

// How long a caller waits for a program that is being built right now (once per version of the helper) before it
// goes on with the hosted helper instead, and how long the build and its trial run may take.
const (
	programWait    = 20 * time.Second
	programBuild   = 2 * time.Minute
	programTrial   = 20 * time.Second
	programVarName = "DECURIX_SCREEN_HELPER"
)

var (
	programMu    sync.Mutex
	programBuilt = map[string]chan struct{}{} // by path: closed when the build of it has ended, well or badly
)

// programPath is where the program for this version of the helper is kept, or "" when this computer has no place
// for it. The name carries a hash of the script, so a new version is never mistaken for an old build.
func programPath() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}

	sum := sha256.Sum256(Script)

	return filepath.Join(local, "Decurix", "bridge", "Screen-"+strings.ToUpper(hex.EncodeToString(sum[:6]))+".exe")
}

// ScreenProgram is the helper as a program of its own, ready to be started with "-DesktopName <name> -Script
// <script>", or "" when the hosted helper is to be used. powershell and script are what the hosted helper is run
// with; hide keeps a console program from showing a window. The first call for a version builds the program and
// tries it; that call and any made meanwhile wait a little for it, so only the very first desktop after an install
// can start hosted.
func ScreenProgram(powershell, script string, hide func(*exec.Cmd)) string {
	if strings.EqualFold(os.Getenv(programVarName), "powershell") {
		return ""
	}

	exe := programPath()
	if exe == "" || powershell == "" {
		return ""
	}

	if usable(exe) {
		return exe
	}
	if _, err := os.Stat(exe + ".bad"); err == nil {
		return "" // it was built and could not be run here: not tried again for this version
	}

	programMu.Lock()
	done, building := programBuilt[exe]
	if !building {
		done = make(chan struct{})
		programBuilt[exe] = done
		go func() {
			defer close(done)
			buildProgram(exe, powershell, script, hide)
		}()
	}
	programMu.Unlock()

	select {
	case <-done:
	case <-time.After(programWait):
	}

	if usable(exe) {
		return exe
	}

	return ""
}

// usable reports whether the program is there and has been seen to run.
func usable(exe string) bool {
	if _, err := os.Stat(exe); err != nil {
		return false
	}
	_, err := os.Stat(exe + ".ok")

	return err == nil
}

// buildProgram has the script compile the program, runs it once to see that this computer lets it run, and marks it
// usable or not. Programs of other versions are cleared away.
func buildProgram(exe, powershell, script string, hide func(*exec.Cmd)) {
	run := func(limit time.Duration, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()

		cmd := exec.CommandContext(ctx, name, args...)
		if hide != nil {
			hide(cmd)
		}

		return cmd.Output()
	}

	if _, err := os.Stat(exe); err != nil {
		if _, err := run(programBuild, powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script, "-DesktopName", "build", "-Build", exe); err != nil {
			return // nothing was made: tried again the next time the control plane starts
		}
	}

	out, err := run(programTrial, exe, "--selftest")
	if err != nil || !bytes.HasPrefix(bytes.TrimSpace(out), []byte("OK")) {
		_ = os.WriteFile(exe+".bad", []byte("the program did not run\n"), 0o644)
		return
	}
	_ = os.WriteFile(exe+".ok", []byte("ok\n"), 0o644)

	// Older versions' programs, and what was written beside them.
	old, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), "Screen-*"))
	for _, file := range old {
		if !strings.HasPrefix(file, exe) {
			_ = os.Remove(file) // one still running cannot be removed, and is tried again after the next build
		}
	}
}
