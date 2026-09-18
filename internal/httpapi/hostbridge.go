package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/desktop"
)

// detectHostAppsHandler returns the detected applications, session stores,
// and developer tokens found on the host machine.
func (s *Server) detectHostAppsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	items := desktop.DetectHostApps()
	jsonResponse(w, items)
}

// hostBridgeExecRequest represents a command execution request sent from inside
// the VM container to be executed on the normal host desktop.
type hostBridgeExecRequest struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
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
		if argsVal := r.FormValue("args"); argsVal != "" {
			_ = json.Unmarshal([]byte(argsVal), &req.Args)
		}
	}

	if strings.TrimSpace(req.Cmd) == "" {
		http.Error(w, "cmd cannot be empty", http.StatusBadRequest)
		return
	}

	// Determine if this is a GUI app that should be launched asynchronously
	isGUIApp := false
	cmdLower := strings.ToLower(req.Cmd)
	for _, guiPrefix := range []string{"google-chrome", "chromium", "firefox", "code", "x-terminal-emulator", "xdg-open", "open"} {
		if strings.Contains(cmdLower, guiPrefix) {
			isGUIApp = true
			break
		}
	}

	if isGUIApp {
		// Launch detached GUI process on the host desktop
		cmd := exec.Command(req.Cmd, req.Args...)
		cmd.Env = os.Environ()
		// Ensure DISPLAY is set for Linux desktop GUI apps
		if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" {
			cmd.Env = append(cmd.Env, "DISPLAY=:0")
		}

		if err := cmd.Start(); err != nil {
			log.Printf("host-bridge: failed to launch GUI app %s: %v", req.Cmd, err)
			jsonResponse(w, hostBridgeExecResponse{
				Success:  false,
				ExitCode: 1,
				Error:    err.Error(),
			})
			return
		}

		go func() {
			_ = cmd.Wait()
		}()

		jsonResponse(w, hostBridgeExecResponse{
			Success:  true,
			Stdout:   fmt.Sprintf("Launched %s on host desktop (pid %d)\n", req.Cmd, cmd.Process.Pid),
			ExitCode: 0,
		})
		return
	}

	// For CLI utilities, execute and capture output with a 30s timeout
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Cmd, req.Args...)
	cmd.Env = os.Environ()
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

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("cmd.exe", "/c", "start", "", target)
	default:
		// Linux
		cmd = exec.Command("xdg-open", target)
		if os.Getenv("DISPLAY") == "" {
			cmd.Env = append(os.Environ(), "DISPLAY=:0")
		}
	}

	if err := cmd.Start(); err != nil {
		log.Printf("host-bridge: open %s failed: %v", target, err)
		http.Error(w, "could not open target on host: "+err.Error(), http.StatusInternalServerError)
		return
	}

	go func() {
		_ = cmd.Wait()
	}()

	jsonResponse(w, map[string]interface{}{
		"success": true,
		"target":  target,
	})
}

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
		http.Error(w, "clipboard copy failed: "+err.Error(), http.StatusInternalServerError)
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
