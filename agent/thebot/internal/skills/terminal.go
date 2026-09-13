package skills

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"thebot/internal/safety"
)

const terminalTimeout = 120 * time.Second

var (
	fencedRe = regexp.MustCompile("(?s)```(?:bash|sh|shell|zsh)?\n(.*?)```")
	inlineRe = regexp.MustCompile("`([^`]+)`")
	// Plain "run the command: X" with no backticks at all. This is how
	// people actually type it, and - worse - it is the exact phrasing the
	// planner is instructed to emit, so a planned terminal step used to
	// fail every time with "could not determine a real command to run"
	// while the plan itself looked perfect.
	cmdLeadRe = regexp.MustCompile(`(?i)\b(?:run|execute)\b(?:\s+the)?(?:\s+(?:command|shell command|script))?\s*:?\s+(.+)$`)
	// Strips a sentence-ending period only. A blanket TrimRight(".") turned
	// "go test ./..." into "go test ./" - the dots there are the argument.
	sentencePeriodRe = regexp.MustCompile(`([A-Za-z0-9])\.$`)
	// A planned step sometimes carries the model's own narrated caveat
	// inline - "pip install -r requirements.txt (assuming a
	// requirements.txt file exists)" ran EXACTLY that, parenthetical and
	// all, as the literal shell command, and failed with exit 2 because
	// "(assuming ...)" isn't valid shell syntax. Real shell parens
	// (subshells, grouping) essentially never open with an English
	// narrating word like this, so it's a safe, narrow signal to strip.
	trailingCommentRe = regexp.MustCompile(`(?i)\s*\((?:assuming|if|since|note|this|that|will|should|may|might|because|in case)\b[^()]*\)\s*$`)

	// Pronouns are the trap here: "run it again" reaches this skill too,
	// and "it again" must never be handed to a shell.
	pronounFirstWord = map[string]bool{
		"it": true, "that": true, "this": true, "them": true,
		"these": true, "those": true, "again": true, "the": true,
	}
	// Single-word commands that are genuinely commands. Needed because the
	// general rule below demands either a path-ish token or 2+ words.
	knownBareCommands = map[string]bool{
		"ls": true, "pwd": true, "whoami": true, "date": true, "uname": true,
		"df": true, "ps": true, "env": true, "hostname": true, "id": true,
	}
)

// looksLikeCommand decides whether free text after "run:" is really a
// command rather than leftover prose. Deliberately conservative - the
// cost of a false positive is executing a sentence in a shell.
func looksLikeCommand(s string) bool {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return false
	}
	first := strings.ToLower(fields[0])
	if pronounFirstWord[first] {
		return false
	}
	// A path-ish first token ("./x.sh", "python3", "avl_tree.py") is a
	// strong signal on its own.
	pathish := strings.ContainsAny(first, "/.")
	return len(fields) >= 2 || pathish || knownBareCommands[first]
}

// TerminalSkill runs a real shell command in the workspace directory -
// real stdout/stderr/exit code, never a claimed success without checking
// the real exit code. Safety boundary is structural (non-root container
// user, cwd confinement to the workspace) rather than command filtering,
// which would be both bypassable and block legitimate use.
type TerminalSkill struct{}

func NewTerminal() *TerminalSkill { return &TerminalSkill{} }

func (t *TerminalSkill) Name() string { return "terminal" }
func (t *TerminalSkill) Description() string {
	return "Runs a real shell command in the workspace directory - real stdout/stderr/exit code, never a claimed success without checking it."
}

func (t *TerminalSkill) Matches(subtask Subtask) bool {
	return subtask.Type == "terminal"
}

// stripWrappingQuotes removes a matching quote pair only when it
// symmetrically wraps the ENTIRE string ("ls -la" -> ls -la). Real bug
// this replaces: strings.Trim(s, `"'`) strips independently from each
// end - "echo 'has $pecial & chars!'" has no leading quote to strip but
// DOES have a trailing one, so Trim removed only that one, silently
// producing an unterminated quote ("echo 'has $pecial & chars!") that
// failed at the shell with a real syntax error. The same broke any
// command ending in a quoted last argument: mv a 'b c.txt', echo "text",
// grep "pattern" file - all extremely ordinary shapes. Only strip when
// both ends genuinely match; never touch one end alone.
func stripWrappingQuotes(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if first == last && (first == '"' || first == '\'' || first == '`') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func extractCommand(step string) (command, source string) {
	if m := fencedRe.FindStringSubmatch(step); m != nil {
		return strings.TrimSpace(m[1]), "explicit (fenced block in the request)"
	}
	if m := inlineRe.FindStringSubmatch(step); m != nil {
		return strings.TrimSpace(m[1]), "explicit (backticks in the request)"
	}
	// Only after the explicit forms, and only when what follows really
	// looks like a command.
	if m := cmdLeadRe.FindStringSubmatch(step); m != nil {
		candidate := stripWrappingQuotes(strings.TrimSpace(m[1]))
		candidate = trailingCommentRe.ReplaceAllString(candidate, "")
		candidate = sentencePeriodRe.ReplaceAllString(candidate, "$1")
		if looksLikeCommand(candidate) {
			return candidate, "named after 'run the command:' in the request"
		}
	}
	// Last resort: no explicit marker at all. looksLikeCommand alone is
	// too loose to gate raw text on its own - it was only ever a
	// secondary filter applied AFTER "run the command:" already
	// established intent, and "please do something vague with no
	// backticks" satisfies it (2+ words, no leading pronoun) just as
	// happily as a real command does; accepting it here really did
	// execute that sentence in a shell (bash: line 1: please: command not
	// found) the first time this was tried. A bare fallback needs its own
	// stricter, unambiguous shell-syntax signal instead.
	//
	// Real bug this fixes: the failure-reasoning model's RETRY_STEP
	// routinely drops the leading "run the command:" phrasing on a retry
	// ("run the command: pip install X" becomes just "python3 -m venv
	// ... && pip install X"). Without this, extractCommand found nothing,
	// the retry silently fell through to a different skill entirely, and
	// the real command the retry was supposed to run never actually ran.
	trimmed := stripWrappingQuotes(strings.TrimSpace(step))
	if looksLikeShellSyntax(trimmed) {
		return trimmed, "bare command text (no explicit marker, but already classified as terminal work)"
	}
	return "", ""
}

// shellSyntaxRe requires an unambiguous shell-syntax marker - a
// chain/pipe/subshell operator, a flag, or a path/extension - none of
// which show up in an ordinary English sentence. Deliberately stricter
// than looksLikeCommand: this is the ONLY gate on text with no "run the
// command:" framing at all, so a false positive here means running a
// sentence as a shell command.
var shellSyntaxRe = regexp.MustCompile(`&&|\|\||[|;]|\$\(|^\./|\s-{1,2}[a-zA-Z]|\.\w{1,4}\b`)

func looksLikeShellSyntax(s string) bool {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return false
	}
	first := strings.ToLower(fields[0])
	if pronounFirstWord[first] || knownBareCommands[first] {
		// knownBareCommands are unambiguous by themselves already -
		// looksLikeCommand (used by the "run the command:" path) covers
		// them; this stricter bare path still requires a real syntax
		// marker beyond just the word, so "ls" alone (with nothing else
		// marking it as a command) stays excluded here.
		return knownBareCommands[first] && len(fields) == 1
	}
	return shellSyntaxRe.MatchString(s)
}

func (t *TerminalSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	command, source := extractCommand(subtask.Step)
	if command == "" {
		return Result{
			Subtask: subtask, SkillName: t.Name(),
			Output:   "Could not determine a real command to run - name it explicitly (e.g. in backticks: `python3 script.py`).",
			Verified: false,
			Meta:     map[string]any{},
		}, nil
	}

	// The same gate the operator's loop goes through: a command written
	// into the request (or by a planner) is exactly as consequential as
	// one a model chose mid-loop.
	if concern := safety.AssessCommand(command); concern != nil && !rc.IsApproved(command) {
		rc.Logf("terminal", "approval needed before `%s` - %s", command, concern.Reason)
		return NeedsApproval(subtask, t.Name(), command, concern.Reason, nil), nil
	}

	rc.Logf("terminal", "command (%s): %s", source, command)
	cwd := rc.Dir()
	rc.Logf("terminal", "cwd: %s", cwd)

	ctx, cancel := context.WithTimeout(rc.Ctx, terminalTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = cwd
	start := time.Now()
	stdout, err := cmd.Output()
	duration := time.Since(start).Milliseconds()

	if err != nil {
		stderr := ""
		exitCode := -1
		exitErr, isExitErr := err.(*exec.ExitError)
		if isExitErr {
			stderr = string(exitErr.Stderr)
			exitCode = exitErr.ExitCode()
		}

		// Every command here runs via `bash -c "<command>"`, so bash ITSELF
		// always starts and exits with a real code - "command not found"
		// and "grep found nothing" both come back as *exec.ExitError, just
		// with different codes. Found via a 100+ case terminal battery:
		// `grep zzz_no_match file1.txt` and plain `false` - both correct,
		// honest, complete results - were reported as Verified:false,
		// which triggered a full LLM failure-reasoning-and-retry cycle
		// that then quietly substituted an UNRELATED command that happened
		// to exit 0 ("false" became "ls -l"), discarding what was
		// actually asked.
		//
		// Exit code alone can't distinguish "ran fine, told us something"
		// from "genuinely failed" - both grep's no-match AND pip failing
		// on an externally-managed environment exit with the same code 1.
		// STDERR is the real signal: a well-behaved tool that completes
		// normally and merely reports a negative result (no match, false,
		// a false test) writes NOTHING to stderr - verified live (grep
		// no-match, false, test -f on a missing file: all empty stderr).
		// A tool that genuinely couldn't do what was asked writes a real
		// message there - bash's own "command not found"/"permission
		// denied", pip's multi-line externally-managed-environment block,
		// cat's "No such file or directory". Empty stderr on a nonzero
		// exit is the command's own complete, honest, self-describing
		// result; real stderr is a real problem worth reasoning about.
		// A command that was killed - the timeout, or the job being
		// cancelled - also exits with empty stderr, but it did not "run
		// fine and report a negative result". It did not finish. Exit code
		// -1 is how a signal death shows up here.
		if ctx.Err() != nil || (isExitErr && exitCode < 0) {
			why := "it was killed before finishing"
			switch {
			case errors.Is(rc.Ctx.Err(), context.DeadlineExceeded):
				why = "the job ran out of time"
			case rc.Ctx.Err() != nil:
				why = "the job was cancelled"
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				why = fmt.Sprintf("it was still running after %s and was killed", terminalTimeout)
			}
			rc.Logf("terminal", "did not finish - %s", why)
			out := fmt.Sprintf("Command: %s\nWorking directory: %s\nDid not finish: %s", command, cwd, why)
			if len(stdout) > 0 {
				out += "\n\nStdout so far:\n" + string(stdout)
			}
			return Result{
				Subtask: subtask, SkillName: t.Name(), Output: out, Verified: false,
				Meta:       map[string]any{"command": command, "source": source, "exitCode": exitCode, "cwd": cwd, "error": why},
				DurationMs: duration,
			}, nil
		}

		if isExitErr && strings.TrimSpace(stderr) == "" {
			rc.Logf("terminal", "exit code: %d (real result, not a failure)", exitCode)
			out := fmt.Sprintf("Command: %s\nWorking directory: %s\nExit code: %d", command, cwd, exitCode)
			if len(stdout) > 0 {
				out += "\n\nStdout:\n" + string(stdout)
			}
			return Result{
				Subtask: subtask, SkillName: t.Name(), Output: out, Verified: true,
				Meta:       map[string]any{"command": command, "source": source, "exitCode": exitCode, "cwd": cwd},
				DurationMs: duration,
			}, nil
		}

		rc.Logf("terminal", "FAILED - exit code: %d - %s", exitCode, err.Error())
		out := fmt.Sprintf("Command: %s\nWorking directory: %s\nExit code: %d (FAILED)", command, cwd, exitCode)
		if stderr != "" {
			out += "\n\nStderr:\n" + stderr
		} else {
			out += "\n\nError: " + err.Error()
		}
		return Result{
			Subtask: subtask, SkillName: t.Name(), Output: out, Verified: false,
			Meta:       map[string]any{"command": command, "source": source, "exitCode": exitCode, "cwd": cwd, "error": err.Error()},
			DurationMs: duration,
		}, nil
	}

	rc.Logf("terminal", "exit code: 0")
	out := fmt.Sprintf("Command: %s\nWorking directory: %s\nExit code: 0 (success)", command, cwd)
	if len(stdout) > 0 {
		out += "\n\nStdout:\n" + string(stdout)
	}
	return Result{
		Subtask: subtask, SkillName: t.Name(), Output: out, Verified: true,
		Meta:       map[string]any{"command": command, "source": source, "exitCode": 0, "cwd": cwd},
		DurationMs: duration,
	}, nil
}
