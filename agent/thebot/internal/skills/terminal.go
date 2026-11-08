package skills

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"thebot/internal/hostshell"
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
	// What a person says after the command is for TheBot, not the shell:
	// "run curl -sI https://example.com in the terminal and tell me the
	// content-type header" ran as that whole sentence, live, and failed with
	// exit 127. Where to run it and what to report are cut off; neither
	// phrase is ever part of a real command line.
	trailingProseRe = regexp.MustCompile(`(?i)(?:\s+(?:in|on|from|using|via|inside)\s+(?:the\s+|a\s+|my\s+)?(?:terminal|shell|command[\s-]?line|console)\b|(?:\s*,\s*|\s+)(?:and|then)\s+(?:tell|show|give|report|let\s+me\s+know|say|explain|summari[sz]e|print\s+out)\b).*$`)

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
// the real exit code. A command goes through the same guard rail the
// operator's do (safety.AssessCommand: a person's yes for what it flags),
// which is a rail, not a boundary - the boundary is the OS (the gate user,
// docs/gate-boundary.md). Review T-26: this said the opposite.
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
		candidate = stripWrappingQuotes(strings.TrimSpace(trailingProseRe.ReplaceAllString(candidate, "")))
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

// commandNotFound is the shell's exit code for a program that is not installed.
const commandNotFound = 127

// missingProgram names the program a command-not-found failure is about: the
// one the shell named, else the command's first word.
func missingProgram(stderr, command string) string {
	if m := missingProgramRe.FindStringSubmatch(stderr); m != nil {
		if m[1] != "" {
			return m[1]
		}
		return m[2]
	}
	if fields := strings.Fields(command); len(fields) > 0 {
		return fields[0]
	}
	return command
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
	// Everything this skill emits about the command goes through the operator's one filter (withholdFor).
	clean := withholdIn(rc, subtask.Step, command)
	cwd := rc.Dir()
	// Read against the folder it runs in: a relative path means what it means there.
	if concern := safety.AssessCommand(command, cwd); concern != nil && !rc.IsApproved(command) {
		rc.Logf("terminal", "approval needed before `%s` - %s", clean(command), concern.Reason)
		res := NeedsApproval(subtask, t.Name(), command, concern.Reason, nil)
		res.Meta["actionShown"] = clean(command)
		res.Output = fmt.Sprintf("Paused before `%s` - %s. Waiting for approval.", clean(command), concern.Reason)
		return res, nil
	}

	rc.Logf("terminal", "command (%s): %s", source, clean(command))
	rc.Logf("terminal", "cwd: %s", cwd)

	ctx, cancel := context.WithTimeout(rc.Ctx, terminalTimeout)
	defer cancel()

	cmd := hostshell.CommandContext(ctx, command)
	cmd.Dir = cwd
	// Soul's own secrets are not the command's (review T-16), and what it prints is held to a bound (T-14).
	cmd.Env = hostshell.ChildEnv()
	var outBuf, errBuf hostshell.CappedBuffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	start := time.Now()
	err := cmd.Run()
	stdout := outBuf.Bytes()
	duration := time.Since(start).Milliseconds()
	// The command itself finished with 0; a background child it started is
	// what still held the pipe (and was cut off). Its own exit is the result.
	err = hostshell.Exited(cmd, err)

	if err != nil {
		stderr := ""
		exitCode := -1
		exitErr, isExitErr := err.(*exec.ExitError)
		if isExitErr {
			stderr = errBuf.String()
			exitCode = exitErr.ExitCode()
		}

		// Every command here runs via `bash -c "<command>"`, so bash itself
		// always starts and exits with a real code. A nonzero exit is not a
		// success, whatever stderr says: `grep` finding nothing and `false`
		// exit nonzero with empty stderr, and so do a failed test and a tool
		// that reports its errors on stdout. The exit code is reported as
		// it is; the result is not verified (see the empty-stderr case
		// below, which also stops the scheduler from swapping in another
		// command that happens to exit 0).
		// A command that was killed - the timeout, or the job being
		// cancelled - also exits with empty stderr, but it did not "run
		// fine and report a negative result". It did not finish. Exit code
		// -1 is how a signal death shows up here.
		// Decurix's screen gate stopped an on-screen action (`gui click` on a
		// Send button: exit 3, nothing happened). That is a pause for a
		// person's yes, exactly as it is in the operator's loop - found
		// live: run from here it was reported as the command's ordinary
		// result, the task ended, and the yes had nothing to resume. The
		// approved command is run again, and the gate lets that one through.
		if reason := screenApprovalReason(command, shellResult{ExitCode: exitCode, Output: string(stdout)}); reason != "" {
			rc.Logf("terminal", "approval needed on screen - %s", reason)
			return NeedsApproval(subtask, t.Name(), command, reason, nil), nil
		}

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

		if answer := negativeAnswer(command, exitCode); isExitErr && answer != "" {
			// grep finding nothing, diff finding a difference, a false test:
			// exit 1 is that program's answer, decided by the program that
			// ran last, not by what it printed. The step's question is
			// answered - "no match" - which is neither the work done nor a
			// failure to retry with another command.
			rc.Logf("terminal", "exit code: 1 - answered: %s", clean(answer))
			out := fmt.Sprintf("Command: %s\nWorking directory: %s\nExit code: 1 - answered: %s", command, cwd, answer)
			if len(stdout) > 0 {
				out += "\n\nStdout:\n" + string(stdout)
			}
			if strings.TrimSpace(stderr) != "" {
				out += "\n\nStderr:\n" + stderr
			}
			return Result{
				Subtask: subtask, SkillName: t.Name(), Output: out, Verified: true,
				Meta:       map[string]any{"command": command, "source": source, "exitCode": exitCode, "cwd": cwd, "answered": answer},
				DurationMs: duration,
			}, nil
		}

		if isExitErr && strings.TrimSpace(stderr) == "" && exitCode != commandNotFound {
			// Nonzero with nothing on stderr: the command ran to its end and
			// said "no" (no match, a false test) - or failed quietly. Either
			// way it is not a success. It is reported as it is, not verified,
			// and marked fatal so the scheduler's recovery does not swap in
			// an unrelated command that exits 0 (found live: `false` became
			// `ls -l`, and the request was lost).
			rc.Logf("terminal", "exit code: %d (nonzero - not a success)", exitCode)
			out := fmt.Sprintf("Command: %s\nWorking directory: %s\nExit code: %d (nonzero: not a success; it printed no error)", command, cwd, exitCode)
			if len(stdout) > 0 {
				out += "\n\nStdout:\n" + string(stdout)
			}
			return Result{
				Subtask: subtask, SkillName: t.Name(), Output: out, Verified: false,
				Meta: map[string]any{"command": command, "source": source, "exitCode": exitCode, "cwd": cwd, "fatal": true,
					"failureReasoning": fmt.Sprintf("`%s` exited with code %d and printed no error: a negative answer or a quiet failure, not a success", command, exitCode)},
				DurationMs: duration,
			}, nil
		}

		// 127 is the shell saying the program is not installed. That is not
		// something a cleverer command line fixes, and the one "fix" a model
		// reaches for - install it - cannot work here (no sudo). Measured
		// live: `curl` missing, the failure reasoning chose
		// `sudo apt-get install curl`, and on the next run asked the person
		// what to do. The operator works the same goal out command by
		// command, told the program is not there.
		if exitCode == commandNotFound {
			program := missingProgram(stderr, command)
			rc.Logf("terminal", "`%s` is not installed here - working it out another way", program)
			return Result{
				Subtask: subtask, SkillName: t.Name(),
				Output:   fmt.Sprintf("Command: %s\nWorking directory: %s\nExit code: 127 (`%s` is not installed)", command, cwd, program),
				Verified: false,
				Meta: map[string]any{"command": command, "source": source, "exitCode": exitCode, "cwd": cwd,
					"reroute":     "operator",
					"rerouteNote": fmt.Sprintf("`%s` is not installed on this machine and programs cannot be installed (no administrator rights). Do it with a program that is here - python3 always is, with pandas, requests and the document libraries; a missing Python library can be added with `pip install --user NAME`.", program)},
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

// negativeAnswers are the programs whose exit 1 is, by convention, an answer
// rather than an error (an error is 2 or more): what that answer says.
var negativeAnswers = map[string]string{
	"grep": "no match", "egrep": "no match", "fgrep": "no match", "zgrep": "no match", "rg": "no match",
	"diff": "the inputs differ", "cmp": "the inputs differ",
	"test": "the test is false", "[": "the test is false", "[[": "the test is false",
}

// negativeAnswer is the answer exit code 1 gives for command, judged by the
// program the shell ran last (the first word of the command line's last
// stage, past variable assignments and wrappers like env or time), or ""
// when exit 1 from that program is not an answer and stays a failure.
func negativeAnswer(command string, exitCode int) string {
	if exitCode != 1 {
		return ""
	}
	segs := shellSegments(command)
	if len(segs) == 0 {
		return ""
	}
	words := strings.Fields(segs[len(segs)-1].Text)
	for len(words) > 0 {
		w := words[0]
		switch {
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "="):
			words = words[1:] // FOO=bar grep ...
			continue
		case w == "env" || w == "command" || w == "builtin" || w == "time" || w == "nice" || w == "exec":
			words = words[1:]
			continue
		}
		break
	}
	if len(words) == 0 {
		return ""
	}
	w := words[0]
	if i := strings.LastIndexByte(w, '/'); i >= 0 && w != "/" {
		w = w[i+1:]
	}
	return negativeAnswers[w]
}
