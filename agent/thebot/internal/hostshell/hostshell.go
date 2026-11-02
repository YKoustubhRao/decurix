// Package hostshell runs a command line in the desktop's shell (bash), in a
// process group of its own so that everything it started can be killed.
package hostshell

import (
	"context"
	"os/exec"
	"time"
)

// waitDelay bounds how long Wait keeps waiting for output pipes after the
// command itself has ended or been killed: a backgrounded child that still
// holds stdout open (`python3 -m http.server &`) must not keep a caller
// waiting until its own timeout, or forever.
const waitDelay = 2 * time.Second

// Command is line as a not-yet-started command in this machine's shell.
func Command(line string) *exec.Cmd {
	name, args := argv(line)
	cmd := exec.Command(name, args...)
	prepare(cmd)
	return cmd
}

// CommandContext is Command in a process group of its own, and when ctx ends
// the whole group is killed - not only the shell. Killing the shell alone
// left its children running (a build's compiler, `npm start`'s node), still
// holding the output pipes, and Wait blocked on them past every timeout.
func CommandContext(ctx context.Context, line string) *exec.Cmd {
	name, args := argv(line)
	cmd := exec.CommandContext(ctx, name, args...)
	prepare(cmd)
	OwnGroup(cmd)
	cmd.Cancel = func() error {
		KillTree(cmd)
		return nil
	}
	cmd.WaitDelay = waitDelay
	return cmd
}
