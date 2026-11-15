// Pipe status: the exit status of every program in the last pipeline a command line ran.
//
// A pipeline's own exit status is its last program's: in `failing_check | tail -5` bash reports tail's 0, and a
// caller reading only the exit code takes a failure it never saw succeed as a success. TrackPipeStatus has bash
// write PIPESTATUS to a file when the command line ends, without changing the line itself: a BASH_ENV file sets an
// EXIT trap before the line runs, then unsets BASH_ENV so the line's own child shells do not inherit it. Text added
// to the line would have been read as part of an unclosed here-document, and a syntax error would have echoed it.
// Technique from hermes-agent agent/verification_evidence.py (_segment_status): a command's exit code counts only
// when the shell proves it. Not a boundary: the command runs as the same user and could rewrite either file; it is
// evidence against an honest mistake, not against a hostile command.
package hostshell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// pipeStatusVar names the file the EXIT trap writes PIPESTATUS to; the rc file unsets it at once.
const pipeStatusVar = "THEBOT_PIPESTATUS_FILE"

// pipeStatusRC is the BASH_ENV file's text. PIPESTATUS must be read first in the trap: any command before it
// replaces it. The trap stays unset when the variable is missing, so a stray BASH_ENV in a child does nothing.
const pipeStatusRC = `# Written by thebot (hostshell.TrackPipeStatus): report the exit status of each program in the last pipeline.
__thebot_ps_file=${` + pipeStatusVar + `-}
unset BASH_ENV ` + pipeStatusVar + `
if [ -n "$__thebot_ps_file" ]; then
  trap '__thebot_ps="${PIPESTATUS[*]}"; printf "%s" "$__thebot_ps" 2>/dev/null >"$__thebot_ps_file"' EXIT
fi
`

var (
	rcOnce sync.Once
	rcPath string
)

// rcFile is the BASH_ENV file, written once per process; "" when it could not be written.
func rcFile() string {
	rcOnce.Do(func() {
		f, err := os.CreateTemp("", "thebot-pipestatus-*.sh")
		if err != nil {
			return
		}
		_, werr := f.WriteString(pipeStatusRC)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(f.Name())
			return
		}
		rcPath = f.Name()
	})
	return rcPath
}

// PipeStatus is where one command's pipeline statuses are written. A nil *PipeStatus is valid and knows nothing.
type PipeStatus struct {
	path string
}

// TrackPipeStatus arranges for cmd to report the status of each program in the last pipeline it ran. Call it after
// cmd.Env is set and before Start; read Statuses after Wait, then Close. It returns nil, and changes nothing, when
// the files cannot be made: the status is then simply unknown, as it was before.
func TrackPipeStatus(cmd *exec.Cmd) *PipeStatus {
	rc := rcFile()
	if rc == "" || cmd == nil {
		return nil
	}
	f, err := os.CreateTemp("", "thebot-ps-*")
	if err != nil {
		return nil
	}
	_ = f.Close()
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	kept := make([]string, 0, len(env)+2)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "BASH_ENV" || name == pipeStatusVar {
			continue
		}
		kept = append(kept, kv)
	}
	cmd.Env = append(kept, "BASH_ENV="+filepath.ToSlash(rc), pipeStatusVar+"="+filepath.ToSlash(f.Name()))
	return &PipeStatus{path: f.Name()}
}

// Statuses is the exit status of each program of the last pipeline, first to last, or nil when the shell did not
// report them: the line replaced the EXIT trap with its own, exec'd another program, or was killed.
func (p *PipeStatus) Statuses() []int {
	if p == nil {
		return nil
	}
	data, err := os.ReadFile(p.path)
	if err != nil {
		return nil
	}
	return parseStatuses(string(data))
}

// Close removes the status file.
func (p *PipeStatus) Close() {
	if p != nil {
		_ = os.Remove(p.path)
	}
}

// parseStatuses reads "1 0 0" into [1 0 0]; anything else is nil, never a guess.
func parseStatuses(s string) []int {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || n > 255 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// sigpipe is the status of a program killed by SIGPIPE (128+13): a later program stopped reading (`yes | head -1`).
// That is how a pipe normally ends early, not a failure of the work.
const sigpipe = 141

// HiddenFailure is the rightmost program of the last pipeline whose own failure the pipeline's exit status hid:
// its position (0-based) and status. ok is false when nothing was hidden - fewer than two programs, the line's exit
// code is already nonzero, the statuses do not belong to the line's final pipeline (its last status is not the exit
// code), or every earlier program succeeded or ended by SIGPIPE. answered says whether a nonzero status is the
// program's answer rather than a failure (grep finding nothing); nil treats every nonzero status as a failure.
func HiddenFailure(statuses []int, exitCode int, answered func(pos, status int) bool) (pos, status int, ok bool) {
	n := len(statuses)
	if n < 2 || exitCode != 0 || statuses[n-1] != exitCode {
		return 0, 0, false
	}
	for i := n - 2; i >= 0; i-- {
		s := statuses[i]
		if s == 0 || s == sigpipe {
			continue
		}
		if answered != nil && answered(i, s) {
			continue
		}
		return i, s, true
	}
	return 0, 0, false
}
