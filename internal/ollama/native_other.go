//go:build !windows

package ollama

import (
	"errors"
	"os/exec"
)

func hide(*exec.Cmd) {}

// startNative exists only on Windows: everywhere else Ollama is a container
// (the bundled one) or a server the person runs.
func startNative(exe, models, logFile string) error {
	return errors.New("Decurix runs its own Ollama program only on Windows")
}
