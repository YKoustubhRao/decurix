// boundary.go is the operator's tripwire: a command that tries to cross the desktop's boundary (act as another
// user, drive another process's socket or memory, reach the gate, the screen broker or the agent around their own
// tools) is not run, and the task ends there as a boundary attempt, shown to the person with the command.
package skills

import (
	"regexp"
	"strings"
)

// The tripwire is not the boundary and not the frozen command guard (internal/safety): the boundary is the OS (the
// gate user, read-only binds, the input broker; docs/gate-boundary.md), and the guard asks before an action that
// commits. The tripwire reads a command for intent a worker never needs for a person's task, whatever the task: each
// rule names a mechanism of the desktop itself, never a product, a workflow or a word of the request. Anything it
// does not see the OS still refuses; what it does see ends the task, because a worker that tries is not one whose
// "done" can be taken on trust.

// boundaryRule is one way across the boundary and the words the person is told it is.
type boundaryRule struct {
	re   *regexp.Regexp
	what string
}

// boundaryAt is where a program word stands in a shell line: a line's start, after a separator or a subshell opener.
const boundaryAt = "(?m)(?:^|[;&|(`]|\\$\\()[ \\t]*(?:(?:env|exec|nohup|command|timeout[ \\t]+\\S+)[ \\t]+)*"

// boundaryRules are the ways across, each structural.
var boundaryRules = []boundaryRule{
	// Another user: a program that runs as someone else, or sudo asked for another user, group or a login shell.
	{regexp.MustCompile(boundaryAt + `(?:su|runuser|setpriv|nsenter|pkexec|doas|machinectl[ \t]+shell)(?:[ \t]|$)`),
		"run a command as another user"},
	{regexp.MustCompile(boundaryAt + `sudo[ \t]+(?:\S+[ \t]+)*?(?:-[a-zA-Z]*[ugis]\b|--(?:user|group|login|shell)\b)`),
		"run a command as another user"},
	// Another process: attach to it, or read or drive it through /proc (its memory, environment, open files, root).
	{regexp.MustCompile(boundaryAt + `(?:gdb|strace|ltrace|ptrace|gcore)[ \t]+(?:\S+[ \t]+)*?(?:-p|--pid|attach)\b`),
		"attach to another process"},
	{regexp.MustCompile(`/proc/(?:[0-9]+|\*|\$[{(]?\w+[})]?|\[[^\]]*\])/(?:mem|environ|fd|root|cwd|map_files)\b`),
		"read another process's memory, environment or open files"},
	// A socket that is not the worker's to drive: a unix socket opened by hand, or a desktop service's own folder
	// or port on this machine (the gate, the screen broker, the agent, soul, the browser's debugging port).
	{regexp.MustCompile(boundaryAt + `(?:(?:nc|ncat|netcat)[ \t]+(?:\S+[ \t]+)*?-U\b|socat\b[^;&|\n]*\b(?i:unix|abstract)-|curl\b[^;&|\n]*--(?:unix|abstract-unix)-socket\b)`),
		"drive another process's socket"},
	{regexp.MustCompile(`/run/decurix-(?:gate|screen)\b|\bDECURIX_AGENT_TOKEN\b|\bgate\.sock\b|\bgui\.sock\b|\bcdp\.sock\b`),
		"reach the approval gate or the screen around their own tools"},
	{regexp.MustCompile(`(?i)(?:\blocalhost|\b127(?:\.\d{1,3}){3}|\[?::1\]?|\b0\.0\.0\.0):(?:8081|4900|8083|9222)\b`),
		"reach a desktop service or the browser around their own tools"},
	{regexp.MustCompile(`--remote-debugging-(?:port|pipe|address)\b`),
		"reach the browser around its screen"},
}

// boundaryAttempt is what command tries across the desktop's boundary, in the person's words, or "". The text a gui
// type types is left out first (MaskTypedText): typing words is not running them.
func boundaryAttempt(command string) string {
	read := MaskTypedText(command)
	if strings.TrimSpace(read) == "" {
		return ""
	}
	for _, r := range boundaryRules {
		if r.re.MatchString(read) {
			return r.what
		}
	}
	return ""
}

// boundaryStop is what the person is shown when the tripwire ends a task: what was tried and the command, which was
// not run. shown is the command as the step lines show it (withheld).
func boundaryStop(what, shown string) string {
	return "Stopped: boundary attempt. The worker tried to " + what + ", which no task needs, so the command was " +
		"not run and the task ends here; nothing it reported is taken as done.\n$ " + shown
}
