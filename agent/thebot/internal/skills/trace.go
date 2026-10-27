package skills

import (
	"fmt"
	"strings"
)

// The task's trace: the job log a person reads in the chat and the task record keeps. Found live (2026-10-26): it
// held each command's first output line only and a review's point count only, so a run could not be followed or
// audited from it.

// maxTraceChars bounds a command's output in the trace: enough to read a listing or a document's figures back, never
// the whole of a long dump.
const maxTraceChars = 1500

// traceLine is a command's result as the trace keeps it: the exit code, then the output up to maxTraceChars, with how
// many lines were cut.
func traceLine(res shellResult) string {
	out := strings.TrimSpace(res.Output)
	if out == "" {
		return fmt.Sprintf("exit %d · (no output)", res.ExitCode)
	}
	if len(out) <= maxTraceChars {
		return fmt.Sprintf("exit %d · %s", res.ExitCode, out)
	}
	if res.ExitCode != 0 {
		// A failed command's reason is at its end (a chain of steps fails at the last one printed): the head and the
		// tail are kept, the middle cut. Found live (2026-10-26): a fifteen-step fill failed and the trace held only
		// the first steps' "Typed ... Selected ...", never why.
		head, tail := out[:maxTraceChars/3], out[len(out)-2*maxTraceChars/3:]
		cut := strings.Count(out, "\n") - strings.Count(head, "\n") - strings.Count(tail, "\n")
		return fmt.Sprintf("exit %d · %s … (%d lines cut) … %s", res.ExitCode, head, cut, tail)
	}
	cut := out[:maxTraceChars]
	kept := strings.Count(cut, "\n") + 1
	return fmt.Sprintf("exit %d · %s … (+%d more lines)", res.ExitCode, cut, strings.Count(out, "\n")+1-kept)
}

// issueLines joins review points as the trace lists them, each cut to a line.
func issueLines(issues []reviewIssue) string {
	var out []string
	for _, i := range issues {
		out = append(out, truncateText(i.text(), 220))
	}
	return strings.Join(out, "\n- ")
}
