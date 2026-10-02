// Package hostshell is the one place that knows which shell this machine has.
//
// A worker in a Linux desktop runs commands with bash. A native Windows worker
// has no bash: its commands are PowerShell, the shell every Windows has. What
// differs between the two - the program, how a command and everything it
// started is killed - is here, so the skills that run commands stay the same.
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
