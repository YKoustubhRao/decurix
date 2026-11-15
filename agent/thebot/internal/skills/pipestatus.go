// Pipeline failures a zero exit code hid: which program of a command line's last pipeline failed, said to the model
// and to the step's result, so `failing_check | tail -5` is never taken as a check that passed.
package skills

import (
	"fmt"
	"strings"

	"thebot/internal/hostshell"
)

// pipeFailure is a program of the last pipeline that failed while the pipeline's own exit status (its last
// program's) was 0.
type pipeFailure struct {
	Program  string // "" when the program could not be named from the line
	Position int    // 1-based, of Of
	Of       int
	Status   int
}

// shellKeywords end a compound command; a pipeline's segments cannot be named past one (`...; done`).
var shellKeywords = map[string]bool{"done": true, "fi": true, "esac": true, "}": true, ")": true, "then": true, "do": true, "else": true}

// hiddenPipeFailure is the failure in command's last pipeline that exit code 0 hid, from the shell's own statuses
// (hostshell.TrackPipeStatus), or nil. A program whose exit 1 is its answer (grep finding nothing: negativeAnswers)
// is not a failure. The programs are named from the line's last segments only when the line ends in that pipeline;
// otherwise the failure is reported by position, never under a guessed name.
func hiddenPipeFailure(command string, statuses []int, exitCode int) *pipeFailure {
	names := pipelinePrograms(command, len(statuses))
	answered := func(pos, status int) bool {
		return status == 1 && names != nil && negativeAnswers[names[pos]] != ""
	}
	pos, status, ok := hostshell.HiddenFailure(statuses, exitCode, answered)
	if !ok {
		return nil
	}
	f := &pipeFailure{Position: pos + 1, Of: len(statuses), Status: status}
	if names != nil {
		f.Program = names[pos]
	}
	return f
}

// pipelinePrograms names the n programs of command's last pipeline from its last n segments, or nil when they cannot
// be named with confidence: fewer segments than programs, or a segment that is a shell keyword or names no program.
func pipelinePrograms(command string, n int) []string {
	segs := shellSegments(command)
	if n == 0 || len(segs) < n {
		return nil
	}
	names := make([]string, 0, n)
	for _, s := range segs[len(segs)-n:] {
		w := programWord(s.Text)
		if w == "" || shellKeywords[w] {
			return nil
		}
		names = append(names, w)
	}
	return names
}

// programWord is the program a simple command runs: its first word past variable assignments and wrappers like env
// or time, without its directory. "" when there is none.
func programWord(segment string) string {
	words := strings.Fields(segment)
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
	return w
}

// note is the line the model and the step's result read: what the exit code alone would have hidden.
func (f *pipeFailure) note() string {
	who := fmt.Sprintf("program %d of %d in the last pipe", f.Position, f.Of)
	if f.Program != "" {
		who = fmt.Sprintf("`%s` (program %d of %d in the pipe)", f.Program, f.Position, f.Of)
	}
	return fmt.Sprintf("[exit status: the pipe's last program exited 0, but %s exited %d, so this did NOT succeed. "+
		"A pipe reports only its last program's status; the exit code shown is the failing program's.]", who, f.Status)
}
