// Package coding is the agent's real software-engineering harness: when a
// request is to WRITE CODE, this takes over from the generic
// one-subtask-at-a-time loop and runs a genuine build lifecycle instead -
// design an architecture, scaffold a real project, implement each file,
// write and RUN real tests for it, diagnose real failures (from its own
// memory first, the web second), and keep fixing until the code actually
// works or a hard budget is spent.
//
// The whole package rests on one rule the rest of this codebase already
// lives by: never claim something works without running it. Every "done"
// here is backed by a real process exit code from a real toolchain.
package coding

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"thebot/internal/hostshell"
)

// inheritedEnv is the real process environment every command starts from.
// Extra vars are appended after it, so a later duplicate key wins - which
// is how a project-scoped PATH or VIRTUAL_ENV overrides the host's.
func inheritedEnv() []string { return os.Environ() }

// Command timeouts by class of work. A dependency install genuinely takes
// minutes (uv resolving a fresh environment, cargo building a dependency
// tree); a syntax check takes under a second. One global timeout would
// either kill real installs or let a hung test run burn the whole budget.
const (
	quickTimeout   = 90 * time.Second
	testTimeout    = 5 * time.Minute
	installTimeout = 10 * time.Minute
)

// CmdResult is the honest outcome of one real command: what ran, where,
// what it printed, and whether it genuinely succeeded.
type CmdResult struct {
	Command  string
	Dir      string
	Stdout   string
	Stderr   string
	ExitCode int
	OK       bool
	TimedOut bool
	Duration time.Duration
}

// Output is stdout and stderr joined the way a human reads a terminal -
// what gets fed back to the model when something fails, since the real
// error is as often on stdout (pytest, go test) as on stderr.
func (r CmdResult) Output() string {
	var b strings.Builder
	if s := strings.TrimSpace(r.Stdout); s != "" {
		b.WriteString(s)
	}
	if s := strings.TrimSpace(r.Stderr); s != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	return b.String()
}

// Tail returns the last n characters of the combined output. Build and
// test failures put the actual error at the END (the summary line, the
// failing assertion, the compiler's final diagnostic) while the head is
// usually hundreds of lines of progress noise - so when output has to be
// truncated to fit a prompt, the tail is the half worth keeping.
func (r CmdResult) Tail(n int) string {
	out := r.Output()
	if len(out) <= n {
		return out
	}
	return "[...earlier output truncated...]\n" + out[len(out)-n:]
}

// Runner executes real shell commands inside one project directory.
type Runner struct {
	Dir   string
	OnLog func(string)
	// Env is extra environment for every command, as "KEY=value" - used to
	// keep a project's toolchain self-contained (e.g. UV_CACHE_DIR and
	// VIRTUAL_ENV pointing inside the workspace) rather than leaking into
	// whatever the host has configured.
	Env []string
}

func (r *Runner) logf(format string, args ...any) {
	if r.OnLog != nil {
		r.OnLog(fmt.Sprintf(format, args...))
	}
}

// Run executes one command with bash, in the runner's directory.
//
// Everything here goes through `bash -c` rather than exec'ing a binary
// directly, because real toolchain work is genuinely shell-shaped:
// `cd build && cmake ..`, `.venv/bin/python -m pytest -q 2>&1`. What it
// deliberately does NOT do is rely on shell state carrying between calls -
// each Run is its own process, so "source .venv/bin/activate" in one call
// means nothing to the next. Every toolchain in this package therefore
// addresses interpreters by real path (.venv/bin/python), never by
// activating anything. That exact mistake - chaining an activate into a
// one-liner and expecting it to persist - is already recorded as a real
// past failure in the scheduler's recovery reasoning.
func (r *Runner) Run(ctx context.Context, command string, timeout time.Duration) CmdResult {
	start := time.Now()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := hostshell.CommandContext(runCtx, command)
	cmd.Dir = r.Dir
	if len(r.Env) > 0 {
		cmd.Env = append(inheritedEnv(), r.Env...)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(start)

	res := CmdResult{
		Command: command, Dir: r.Dir,
		Stdout: stdout.String(), Stderr: stderr.String(),
		Duration: duration,
	}

	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		res.ExitCode = -1
		res.OK = false
		// A timeout is its own distinct failure and must be reported as
		// one: "no output, exit -1" reads like a crash and sends the fix
		// loop looking for a bug in the code, when the real problem is a
		// command that hangs (a dev server started in the foreground, a
		// test waiting on stdin, an install prompting for confirmation).
		res.Stderr += fmt.Sprintf("\n[timed out after %s - the command did not finish on its own]", timeout)
		r.logf("coding-exec: TIMEOUT after %s: %s", duration.Round(time.Second), truncate(command, 120))

	case err != nil:
		res.OK = false
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
			res.Stderr += "\n" + err.Error()
		}
		r.logf("coding-exec: exit %d (%s): %s", res.ExitCode, duration.Round(time.Millisecond), truncate(command, 120))

	default:
		res.OK = true
		res.ExitCode = 0
		r.logf("coding-exec: ok (%s): %s", duration.Round(time.Millisecond), truncate(command, 120))
	}

	return res
}

// RunAll runs commands in order and stops at the first real failure,
// returning every result so far. Used for setup sequences where a later
// step is meaningless if an earlier one failed (there is no point running
// `uv pip install` when `uv venv` just failed).
func (r *Runner) RunAll(ctx context.Context, commands []string, timeout time.Duration) []CmdResult {
	var out []CmdResult
	for _, c := range commands {
		res := r.Run(ctx, c, timeout)
		out = append(out, res)
		if !res.OK {
			break
		}
	}
	return out
}

// Have reports whether a tool is really present on PATH, by asking the
// shell rather than assuming. Used to choose between real alternatives
// (uv vs pip, cmake vs a bare compiler) from fact instead of hope.
func (r *Runner) Have(ctx context.Context, tool string) bool {
	return r.Run(ctx, "command -v "+tool+" >/dev/null 2>&1", 20*time.Second).OK
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
