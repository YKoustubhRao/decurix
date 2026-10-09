// Package hostshell runs a command line in the desktop's shell (bash), in a
// process group of its own so that everything it started can be killed.
package hostshell

import (
	"context"
	"os/exec"
)

// Command is line as a not-yet-started command in this machine's shell.
func Command(line string) *exec.Cmd {
	name, args := argv(line)
	cmd := exec.Command(name, args...)
	prepare(cmd)
	return cmd
}

// CommandContext is Command, killed when ctx ends.
func CommandContext(ctx context.Context, line string) *exec.Cmd {
	name, args := argv(line)
	cmd := exec.CommandContext(ctx, name, args...)
	prepare(cmd)
	return cmd
}
