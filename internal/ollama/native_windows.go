//go:build windows

package ollama

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// hide keeps a console program from opening a console window.
func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// startNative starts `ollama serve` on NativeURL, with its models in models
// and what it prints in logFile. It is left running when the control plane
// stops: the next start finds it answering and uses it, with the model still
// in memory.
func startNative(exe, models, logFile string) error {
	if err := os.MkdirAll(models, 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(os.Environ(),
		"OLLAMA_HOST=127.0.0.1:11435",
		"OLLAMA_MODELS="+models,
		"OLLAMA_KEEP_ALIVE="+KeepAlive(),
		"OLLAMA_MAX_LOADED_MODELS="+defaultMaxLoadedModels(),
		"OLLAMA_NUM_PARALLEL=1",
	)
	cmd.Stdout = out
	cmd.Stderr = out
	// A process group of its own, so Ctrl+C in a console the control plane
	// was started from does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000200}

	if err := cmd.Start(); err != nil {
		return err
	}

	return cmd.Process.Release()
}
