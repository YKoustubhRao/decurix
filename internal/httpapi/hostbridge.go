package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// findDockerBinary locates an available docker binary on the host or in standard container paths.
func findDockerBinary() string {
	candidates := []string{"docker", "/usr/bin/docker", "/usr/local/bin/docker", "/bin/docker"}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && (fi.Mode()&0111 != 0) {
			return c
		}
	}
	return ""
}

// detectHostAppsHandler returns the detected applications, session stores,
// and developer tokens found on the host machine.
func (s *Server) detectHostAppsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	force := r.URL.Query().Get("refresh") == "1" || r.URL.Query().Get("refresh") == "true"
	// Somebody looking at the apps they can copy is about to create a desktop: get the host ready while they choose.
	desktop.WarmHostApps()
	items := desktop.DetectHostAppsForce(force)
	jsonResponse(w, items)
}

// copyHostAppsHandler copies host apps into a desktop that already exists - the same copy desktop creation does - so apps
// can be added later, and launchers written by an older build (which stop working when the launch mechanism changes) can
// be refreshed without recreating the desktop. Form fields: id, and host_apps as a comma-separated list of IDs from
// /api/host-apps/detect.
func (s *Server) copyHostAppsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	var apps []string
	for _, part := range strings.Split(r.FormValue("host_apps"), ",") {
		if part = strings.TrimSpace(part); part != "" {
			apps = append(apps, part)
		}
	}
	if id == "" || len(apps) == 0 {
		http.Error(w, "id and host_apps are required", http.StatusBadRequest)
		return
	}
	d, err := s.Desktops.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, desktop.ErrNotFound) {
			http.Error(w, "desktop not found", http.StatusNotFound)
			return
		}
		jsonError(w, err)
		return
	}
	if err := s.Desktops.MigrateHostApps(r.Context(), d.ID, apps); err != nil {
		jsonError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"id": d.ID, "host_apps": apps})
}

// hostBridgeExecRequest represents a command execution request sent from inside
// the VM container to be executed on the normal host desktop.
type hostBridgeExecRequest struct {
	Cmd     string   `json:"cmd"`
	Args    []string `json:"args"`
	Display string   `json:"display,omitempty"`
	// Desktop and Token authorise the command: Token is winhost.ExecToken for this desktop, Cmd and Args, which a
	// launcher was written with. Nothing else is run.
	Desktop string `json:"desktop,omitempty"`
	Token   string `json:"token,omitempty"`
}

// hostBridgeExecResponse represents the result of the host command execution.
type hostBridgeExecResponse struct {
	Success  bool   `json:"success"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// hostBridgeExecHandler executes commands directly on the host desktop machine,
// enabling programs to run on the normal desktop with zero duplicate RAM in the VM.
func (s *Server) hostBridgeExecHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req hostBridgeExecRequest
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		req.Cmd = r.FormValue("cmd")
		req.Display = r.FormValue("display")
		req.Desktop = r.FormValue("desktop")
		req.Token = r.FormValue("token")
		if argsVal := r.FormValue("args"); argsVal != "" {
			_ = json.Unmarshal([]byte(argsVal), &req.Args)
		}
	}

	if strings.TrimSpace(req.Cmd) == "" {
		http.Error(w, "cmd cannot be empty", http.StatusBadRequest)
		return
	}

	// This runs a program on the host, and the caller may be a desktop whose agent takes instructions from web pages.
	// It only ever runs what a launcher Decurix wrote was signed for - exactly that program and those arguments.
	if !winhost.ValidExecToken(s.hostGUISecretKey(), req.Desktop, req.Cmd, req.Args, req.Token) {
		log.Printf("host-bridge: refused an unsigned or altered command from %s", r.RemoteAddr)
		http.Error(w, "this command is not one a launcher was written to run", http.StatusForbidden)
		return
	}

	// Resolve target display for display tunneling:
	// Direct GUI rendering back to the requesting desktop container instead of the host desktop.
	targetDisplay := strings.TrimSpace(req.Display)
	if targetDisplay == "" {
		if clientHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && clientHost != "" {
			if clientHost != "127.0.0.1" && clientHost != "::1" && clientHost != "localhost" {
				targetDisplay = clientHost + ":1"
			}
		}
	}

	// Determine if this is a GUI app that should be launched asynchronously
	isGUIApp := false
	cmdLower := strings.ToLower(req.Cmd)
	for _, guiPrefix := range []string{
		"google-chrome", "chromium", "firefox", "msedge", "code",
		"x-terminal-emulator", "xdg-open", "open", "cmd.exe", "powershell.exe",
		"explorer.exe", "start", "spotify", "discord", "slack",
	} {
		if strings.Contains(cmdLower, guiPrefix) {
			isGUIApp = true
			break
		}
	}

	if isGUIApp {
		// Launch detached GUI process on the host desktop with tunneled display
		cmd := createHostCmd(nil, targetDisplay, req.Cmd, req.Args...)
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf

		if err := cmd.Start(); err != nil {
			log.Printf("host-bridge: failed to launch GUI app %s: %v", req.Cmd, err)
			jsonResponse(w, hostBridgeExecResponse{
				Success:  false,
				ExitCode: 1,
				Error:    err.Error(),
			})
			return
		}

		pid := cmd.Process.Pid
		go func() {
			err := cmd.Wait()
			if err != nil {
				log.Printf("host-bridge: GUI app %s (pid %d) exited with error: %v, stderr: %s", req.Cmd, pid, err, strings.TrimSpace(stderrBuf.String()))
			}
		}()

		jsonResponse(w, hostBridgeExecResponse{
			Success:  true,
			Stdout:   fmt.Sprintf("Launched %s on host desktop (pid %d)\n", req.Cmd, pid),
			ExitCode: 0,
		})
		return
	}

	// For CLI utilities, execute and capture output with a 30s timeout
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cmd := createHostCmd(ctx, targetDisplay, req.Cmd, req.Args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	jsonResponse(w, hostBridgeExecResponse{
		Success:  err == nil,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Error:    errString(err),
	})
}

// createHostCmd constructs an exec.Cmd that can execute on the real host machine,
// falling back to Docker socket nsenter if running inside an unprivileged/isolated container.
// If display is non-empty, the command is configured to direct its GUI/X11 rendering to that display
// (e.g. tunneling display back to the desktop container's X11 server).
func createHostCmd(ctx context.Context, display, cmdStr string, args ...string) *exec.Cmd {
	// A Windows program is on the host already: run the program itself. There is no bash to wrap it in (and a bash
	// that a WSL install puts on PATH would run it inside Linux), and going without a shell means no quoting to get wrong.
	if runtime.GOOS == "windows" {
		var cmd *exec.Cmd
		if ctx != nil {
			cmd = exec.CommandContext(ctx, cmdStr, args...)
		} else {
			cmd = exec.Command(cmdStr, args...)
		}
		cmd.Stdin = strings.NewReader("")
		return cmd
	}

	fullCmd := strings.TrimSpace(cmdStr)
	if len(args) > 0 {
		var quoted []string
		for _, a := range args {
			quoted = append(quoted, fmt.Sprintf("%q", a))
		}
		fullCmd = fullCmd + " " + strings.Join(quoted, " ")
	}

	cmdBase := strings.ToLower(filepath.Base(cmdStr))
	isWinCmd := strings.HasPrefix(cmdBase, "cmd") ||
		strings.HasPrefix(cmdBase, "powershell") ||
		strings.HasPrefix(cmdBase, "explorer") ||
		strings.HasPrefix(cmdBase, "start") ||
		strings.HasSuffix(cmdBase, ".exe") ||
		strings.HasPrefix(fullCmd, "cmd.exe") ||
		strings.HasPrefix(fullCmd, "powershell.exe") ||
		strings.HasPrefix(fullCmd, "explorer.exe")

	// If it's a Windows command and cmd.exe is not directly executable in PATH (e.g. inside Docker container)
	if isWinCmd && desktop.IsWSLOrWindows() {
		if _, err := exec.LookPath("cmd.exe"); err != nil {
			if _, err := os.Stat("/var/run/docker.sock"); err == nil {
				if dockerBin := findDockerBinary(); dockerBin != "" {
					displayEnv := ""
					if display != "" {
						displayEnv = fmt.Sprintf("DISPLAY=%s ", display)
					}
					nsCmd := fmt.Sprintf("%sPATH=$PATH:/mnt/c/WINDOWS/system32:/mnt/c/Windows/system32:/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0:/mnt/c/Windows/System32/WindowsPowerShell/v1.0:/mnt/c/Windows %s", displayEnv, fullCmd)
					var cmd *exec.Cmd
					if ctx != nil {
						cmd = exec.CommandContext(ctx, dockerBin, "run", "--rm", "--privileged", "--pid=host", "alpine", "nsenter", "-t", "1", "-m", "-u", "-n", "-i", "bash", "-c", nsCmd)
					} else {
						cmd = exec.Command(dockerBin, "run", "--rm", "--privileged", "--pid=host", "alpine", "nsenter", "-t", "1", "-m", "-u", "-n", "-i", "bash", "-c", nsCmd)
					}
					cmd.Stdin = strings.NewReader("")
					return cmd
				}
			}
		}
	}

	var cmd *exec.Cmd
	if ctx != nil {
		cmd = exec.CommandContext(ctx, "bash", "-c", fullCmd)
	} else {
		cmd = exec.Command("bash", "-c", fullCmd)
	}
	cmd.Stdin = strings.NewReader("")
	cmd.Env = os.Environ()
	if isWinCmd && desktop.IsWSLOrWindows() {
		currPath := os.Getenv("PATH")
		winPaths := "/mnt/c/WINDOWS/system32:/mnt/c/Windows/system32:/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0:/mnt/c/Windows/System32/WindowsPowerShell/v1.0:/mnt/c/Windows"
		cmd.Env = append(cmd.Env, fmt.Sprintf("PATH=%s:%s", currPath, winPaths))
	}
	if display != "" {
		cmd.Env = append(cmd.Env, "DISPLAY="+display)
	} else if runtime.GOOS == "linux" && os.Getenv("DISPLAY") != "" {
		cmd.Env = append(cmd.Env, "DISPLAY="+os.Getenv("DISPLAY"))
	}
	return cmd
}

// openURLPattern is what may be opened on the host: an http(s) link with nothing a shell or cmd.exe would act on. The
// target is spliced into a shell command line, so quotes, backslashes, $, backticks, ^, <, >, | and ; are refused
// rather than escaped, and so are spaces and control characters. (& and % are fine inside the quotes it is given.)
var openURLPattern = regexp.MustCompile(`^https?://[A-Za-z0-9._~:/?#@!*+,=&%\[\]()-]+$`)

// safeOpenURL reports whether target is a link hostBridgeOpenHandler will open.
func safeOpenURL(target string) bool {
	return len(target) <= 2048 && openURLPattern.MatchString(target)
}

// hostBridgeOpenRequest represents a request to open a URL or file on the host desktop screen.
type hostBridgeOpenRequest struct {
	Target string `json:"target"`
}

// hostBridgeOpenHandler opens a URL or document on the host desktop using the host OS opener.
func (s *Server) hostBridgeOpenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req hostBridgeOpenRequest
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		req.Target = r.FormValue("target")
	}

	target := strings.TrimSpace(req.Target)
	if target == "" {
		http.Error(w, "target cannot be empty", http.StatusBadRequest)
		return
	}
	if !safeOpenURL(target) {
		http.Error(w, "only plain http(s) links can be opened on the host", http.StatusBadRequest)
		return
	}

	var cmd *exec.Cmd
	switch {
	case runtime.GOOS == "windows":
		// The URL protocol handler, not cmd.exe's start: no shell parses the link, so the & of a query string stays one.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case runtime.GOOS == "darwin":
		cmd = createHostCmd(nil, "", fmt.Sprintf("open %q", target))
	case desktop.IsWSLOrWindows():
		cmd = createHostCmd(nil, "", fmt.Sprintf(`cmd.exe /c start "" %q`, target))
	default:
		cmd = createHostCmd(nil, "", fmt.Sprintf("xdg-open %q", target))
	}

	if err := startAndWatch(cmd, hostOpenWait); err != nil {
		log.Printf("host-bridge: open %s failed: %v", target, err)
		http.Error(w, "could not open target on host: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"success": true,
		"target":  target,
	})
}

// hostOpenWait is how long an opener gets to fail before it is taken to have worked. Openers hand the link to a browser
// and return at once, so one that has not finished by then is a browser that is staying attached, not a failure.
const hostOpenWait = 3 * time.Second

// startAndWatch starts cmd and reports a failure that shows up within wait: it could not be started, or it exited with an
// error (a control plane in a container has no xdg-open, which used to be answered with success). A command still running
// after wait is left to finish on its own.
func startAndWatch(cmd *exec.Cmd, wait time.Duration) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		return nil
	case <-time.After(wait):
		return nil
	}
}

// maxHostClipBytes bounds what a desktop can put on the host's clipboard in one request.
const maxHostClipBytes = 64 << 10

// hostBridgeClipRequest represents a request to copy text into the host OS clipboard.
type hostBridgeClipRequest struct {
	Text string `json:"text"`
}

// hostBridgeClipHandler receives text from the VM and copies it into the host clipboard.
func (s *Server) hostBridgeClipHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxHostClipBytes)

	var req hostBridgeClipRequest
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		req.Text = r.FormValue("text")
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "windows":
		cmd = exec.Command("clip.exe")
	default:
		// Linux: try xclip, fallback to wl-copy or xsel
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else {
			cmd = exec.Command("xsel", "--clipboard", "--input")
		}
		if os.Getenv("DISPLAY") == "" {
			cmd.Env = append(os.Environ(), "DISPLAY=:0")
		}
	}

	cmd.Stdin = strings.NewReader(req.Text)
	if err := cmd.Run(); err != nil {
		log.Printf("host-bridge: clip failed: %v", err)

		// No clipboard tool on this host (a control plane in a container, say)
		// is "not available here", not a server fault.
		if errors.Is(err, exec.ErrNotFound) {
			http.Error(w, "the host clipboard is not available here", http.StatusNotImplemented)
			return
		}

		http.Error(w, "clipboard copy failed", http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"success": true,
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
