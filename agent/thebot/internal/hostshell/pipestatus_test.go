// Tests for pipe status tracking: the real shell reports each program's status, and HiddenFailure finds the
// failure a pipeline's exit code hid.
package hostshell

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// run runs line with pipe status tracked and returns its exit code, statuses and output.
func run(t *testing.T, line string) (int, []int, string) {
	t.Helper()
	cmd := CommandContext(context.Background(), line)
	cmd.Env = ChildEnv()
	ps := TrackPipeStatus(cmd)
	if ps == nil {
		t.Fatal("TrackPipeStatus returned nil")
	}
	defer ps.Close()
	out, err := cmd.CombinedOutput()
	err = Exited(cmd, err)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return code, ps.Statuses(), string(out)
}

// The shell's own exit code for `false | tail -1` is tail's 0; the statuses say false failed.
func TestTrackPipeStatusReportsEveryProgramOfTheLastPipeline(t *testing.T) {
	code, st, _ := run(t, "true; false | cat")
	if st == nil {
		t.Skip("this bash does not run BASH_ENV for -c")
	}
	if code != 0 || !reflect.DeepEqual(st, []int{1, 0}) {
		t.Fatalf("code=%d statuses=%v, want 0 [1 0]", code, st)
	}
	if code, st, _ := run(t, "false | true | true"); code != 0 || !reflect.DeepEqual(st, []int{1, 0, 0}) {
		t.Fatalf("three programs: code=%d statuses=%v", code, st)
	}
	if code, st, _ := run(t, "echo hi"); code != 0 || !reflect.DeepEqual(st, []int{0}) {
		t.Fatalf("one program: code=%d statuses=%v", code, st)
	}
}

// The line itself is unchanged: a here-document left open still reads to the end, $0 is still bash, a syntax error
// does not show the trap, and the line's own child shells do not inherit BASH_ENV.
func TestTrackPipeStatusLeavesTheLineAlone(t *testing.T) {
	if _, st, _ := run(t, "true"); st == nil {
		t.Skip("this bash does not run BASH_ENV for -c")
	}
	if _, _, out := run(t, "cat <<EOF\nabc"); !strings.Contains(out, "abc") || strings.Contains(out, "trap") {
		t.Fatalf("open here-document: %q", out)
	}
	if _, _, out := run(t, "echo $0"); strings.TrimSpace(out) != "bash" && !strings.HasSuffix(strings.TrimSpace(out), "bash") {
		t.Fatalf("$0 = %q", out)
	}
	code, _, out := run(t, "echo bad syntax (")
	if code == 0 || strings.Contains(out, "__thebot") || strings.Contains(out, "trap") {
		t.Fatalf("syntax error: code=%d out=%q", code, out)
	}
	if _, _, out := run(t, `bash -c 'echo "[$BASH_ENV][$`+pipeStatusVar+`]"'`); strings.TrimSpace(out) != "[][]" {
		t.Fatalf("child shell inherited the tracking: %q", out)
	}
}

// A line that sets its own EXIT trap, or ends by exec, reports nothing: unknown, never a guess.
func TestTrackPipeStatusIsUnknownWhenTheLineTakesTheTrap(t *testing.T) {
	if _, st, _ := run(t, "true"); st == nil {
		t.Skip("this bash does not run BASH_ENV for -c")
	}
	if _, st, _ := run(t, `trap "echo mine" EXIT; false | true`); st != nil {
		t.Fatalf("own trap: statuses=%v, want nil", st)
	}
}

// A nil PipeStatus is valid and knows nothing.
func TestNilPipeStatus(t *testing.T) {
	var p *PipeStatus
	if p.Statuses() != nil {
		t.Fatal("nil PipeStatus reported statuses")
	}
	p.Close()
}

// parseStatuses accepts only a list of exit statuses.
func TestParseStatuses(t *testing.T) {
	for in, want := range map[string][]int{
		"1 0": {1, 0}, "0": {0}, " 141 0 \n": {141, 0}, "": nil, "1 x": nil, "-1 0": nil, "300 0": nil,
	} {
		if got := parseStatuses(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseStatuses(%q) = %v, want %v", in, got, want)
		}
	}
}

// HiddenFailure finds the rightmost failing program a zero exit code hid, and nothing else.
func TestHiddenFailure(t *testing.T) {
	cases := []struct {
		st     []int
		code   int
		pos, s int
		ok     bool
	}{
		{[]int{1, 0}, 0, 0, 1, true},
		{[]int{2, 1, 0}, 0, 1, 1, true},
		{[]int{0, 0}, 0, 0, 0, false},
		{[]int{141, 0}, 0, 0, 0, false}, // yes | head -1
		{[]int{0, 1}, 1, 0, 0, false},   // already visible
		{[]int{1}, 0, 0, 0, false},      // not a pipeline
		{[]int{1, 0}, 3, 0, 0, false},   // statuses of an earlier pipeline, not the line's last
		{nil, 0, 0, 0, false},
	}
	for _, c := range cases {
		pos, s, ok := HiddenFailure(c.st, c.code, nil)
		if ok != c.ok || (ok && (pos != c.pos || s != c.s)) {
			t.Errorf("HiddenFailure(%v, %d) = %d %d %v, want %d %d %v", c.st, c.code, pos, s, ok, c.pos, c.s, c.ok)
		}
	}
	answered := func(pos, status int) bool { return pos == 0 && status == 1 }
	if _, _, ok := HiddenFailure([]int{1, 0}, 0, answered); ok {
		t.Error("an answered status was reported as a failure")
	}
}

// The rc file is written once and is the trap text.
func TestRCFile(t *testing.T) {
	p := rcFile()
	if p == "" {
		t.Fatal("no rc file")
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != pipeStatusRC {
		t.Fatalf("rc file: %v %q", err, data)
	}
	if rcFile() != p {
		t.Fatal("rc file written twice")
	}
}
