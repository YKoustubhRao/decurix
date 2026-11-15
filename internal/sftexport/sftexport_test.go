// sftexport_test.go checks the training export: which runs are taken, that only steps labelled ok are kept, that the
// middle turns are the stored ones in order, both formats, and that no secret reaches a line.
package sftexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

func exit(n int) *int { return &n }

// run is a checked-done run: three commands (one failed, which is left out), a question with its answer, a
// screen action no command made.
func run() runs.Trajectory {
	return runs.Trajectory{
		Schema: runs.Schema, RunID: "e_1", TaskID: "t_1", Worker: runs.Worker{ID: "box"},
		Request: runs.Request{Text: "count the rows of report.csv"},
		Verdict: json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"done","by":"verify","basis":["code","model"]}`),
		Labels:  runs.Labels{Run: "done"},
		Steps: []runs.Step{
			{I: 0, At: "2026-07-01T10:00:00Z", Label: "ok", Command: &runs.Command{Text: "ls", Exit: exit(0), OutputHead: "report.csv"}},
			{I: 1, At: "2026-07-01T10:00:05Z", Label: "failed", Command: &runs.Command{Text: "wc -l reprot.csv", Exit: exit(1), OutputHead: "No such file"}},
			{I: 2, At: "2026-07-01T10:00:09Z", Label: "ok", Action: &runs.Action{Kind: "click"}},
			{I: 3, At: "2026-07-01T10:00:20Z", Label: "ok", Command: &runs.Command{Text: "wc -l report.csv", Exit: exit(0), OutputHead: "41 report.csv"}},
		},
		Questions: []runs.Question{{InterventionID: "i_1", AskedAt: "2026-07-01T10:00:10Z", Text: "Count the header too?",
			Answer: &runs.Answer{Text: "no, only data rows"}}},
	}
}

func TestOnlyCheckedDoneRunsAreTaken(t *testing.T) {
	if ok, why := Eligible(run()); !ok {
		t.Fatalf("a code-checked done was refused: %s", why)
	}
	cases := map[string]func(*runs.Trajectory){
		"model only":    func(r *runs.Trajectory) { r.Verdict = json.RawMessage(`{"verdict":"done","basis":["model"]}`) },
		"not done":      func(r *runs.Trajectory) { r.Labels.Run = "unchecked" },
		"marked wrong":  func(r *runs.Trajectory) { r.Labels.PersonVerdict = "wrong" },
		"no verdict":    func(r *runs.Trajectory) { r.Verdict = nil },
		"false done":    func(r *runs.Trajectory) { r.Labels.Eval = json.RawMessage(`{"false_done":true}`) },
		"no bundle":     func(r *runs.Trajectory) { r.Integrity.Missing = []string{"no bundle"} },
		"verdict stops": func(r *runs.Trajectory) { r.Verdict = json.RawMessage(`{"verdict":"stopped","basis":["code"]}`) },
	}
	for name, change := range cases {
		r := run()
		change(&r)
		if ok, _ := Eligible(r); ok {
			t.Errorf("%s: taken", name)
		}
	}
	r := run()
	r.Verdict = json.RawMessage(`{"verdict":"done","basis":["model"]}`)
	r.Labels.PersonVerdict = "correct"
	if ok, why := Eligible(r); !ok {
		t.Fatalf("a model-checked done a person marked correct was refused: %s", why)
	}
}

// The record is the request, then each kept step's command and result and the question with its answer, in time
// order, then DONE: nothing summarised, the failed step and its result left out and counted.
func TestTheProtocolRecordKeepsGoodStepsInOrder(t *testing.T) {
	rec, err := Build(run(), "There are 40 data rows.", FormatDecurix)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{
		{"system", SystemPrompt},
		{"user", "count the rows of report.csv"},
		{"assistant", "COMMAND:\nls"},
		{"user", "Command execution result:\n\nexit_code: 0\n\noutput:\nreport.csv"},
		{"assistant", "HELP:\nCount the header too?"},
		{"user", "The human replied: no, only data rows"},
		{"assistant", "COMMAND:\nwc -l report.csv"},
		{"user", "Command execution result:\n\nexit_code: 0\n\noutput:\n41 report.csv"},
		{"assistant", "DONE:\nThere are 40 data rows."},
	}
	if len(rec.Messages) != len(want) {
		t.Fatalf("got %d messages: %+v", len(rec.Messages), rec.Messages)
	}
	for i := range want {
		if rec.Messages[i] != want[i] {
			t.Errorf("message %d = %+v, want %+v", i, rec.Messages[i], want[i])
		}
	}
	if rec.Dropped != 1 || rec.Unshown != 1 {
		t.Fatalf("dropped %d, without command %d", rec.Dropped, rec.Unshown)
	}
	if _, err := Build(run(), "  ", FormatDecurix); err == nil {
		t.Fatal("a run with no final report was built")
	}
}

// The tool-call variant carries the same turns as <tool_call> and <tool_response> blocks of valid JSON, and says when
// an output was cut at the record's 2 KB.
func TestTheToolCallVariant(t *testing.T) {
	r := run()
	r.Steps[3].Command.OutputHead = strings.Repeat("y", outputHeadBytes)
	rec, err := Build(r, "40 rows", FormatToolCall)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Messages) != 0 || len(rec.Conversations) != 9 || rec.Conversations[0].From != "system" {
		t.Fatalf("turns: %+v", rec.Conversations)
	}
	call := rec.Conversations[2].Value
	if rec.Conversations[2].From != "gpt" || !strings.HasPrefix(call, "<tool_call>\n") || !strings.HasSuffix(call, "\n</tool_call>") {
		t.Fatalf("call turn: %q", call)
	}
	var parsed struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(call, "<tool_call>\n"), "\n</tool_call>")
	if err := json.Unmarshal([]byte(inner), &parsed); err != nil || parsed.Name != "shell" || parsed.Arguments["command"] != "ls" {
		t.Fatalf("call JSON: %q %v", inner, err)
	}
	last := rec.Conversations[7]
	if last.From != "tool" || !strings.Contains(last.Value, "first 2 KB") {
		t.Fatalf("cut output not said: %.120q", last.Value)
	}
	if rec.Conversations[8].Value != "40 rows" {
		t.Fatalf("final turn: %+v", rec.Conversations[8])
	}
}

// A secret answer goes out as the withheld mark, and a value the control plane holds as secret, or one labelled as a
// password, never reaches a line even when it slipped into a command or a report.
func TestNoSecretReachesALine(t *testing.T) {
	const held = "Qx7-held-secret-value-993"
	withhold.HoldAlways("sftexport-test", held)
	defer withhold.ReleaseAlways("sftexport-test")
	r := run()
	r.Questions[0].Secret = true
	r.Questions[0].Answer.Text = "zebra-otp-55120"
	r.Steps[0].Command.Text = "login --token " + held
	r.Steps[3].Command.OutputHead = "password: hunter2hunter2"
	for _, format := range []string{FormatDecurix, FormatToolCall} {
		rec, err := Build(r, "done; the key was "+held, format)
		if err != nil {
			t.Fatal(err)
		}
		line, _ := json.Marshal(rec)
		for _, secret := range []string{held, "zebra-otp-55120", "hunter2hunter2"} {
			if bytes.Contains(line, []byte(secret)) {
				t.Errorf("%s: %q reached the line: %s", format, secret, line)
			}
		}
		if !bytes.Contains(line, []byte(withhold.Secret)) {
			t.Errorf("%s: the secret answer is not marked withheld", format)
		}
	}
}

type fakeSource struct {
	refs []runs.ExecutionRef
	traj map[string]runs.Trajectory
}

func (f fakeSource) Ended(time.Time) ([]runs.ExecutionRef, error) { return f.refs, nil }
func (f fakeSource) Trajectory(_ context.Context, id string) (runs.Trajectory, error) {
	t, ok := f.traj[id]
	if !ok {
		return runs.Trajectory{}, errors.New("no record")
	}
	return t, nil
}
func (f fakeSource) FinalReply(string, string) (string, error) { return "the answer", nil }

// Write writes one line per eligible run and counts the rest by why, never a part line.
func TestWriteIsOneLinePerEligibleRun(t *testing.T) {
	good, unchecked := run(), run()
	unchecked.RunID, unchecked.Labels.Run = "e_2", "unchecked"
	src := fakeSource{
		refs: []runs.ExecutionRef{{ExecID: "e_1", TaskID: "t_1", Worker: "box"}, {ExecID: "e_2", TaskID: "t_2", Worker: "box"},
			{ExecID: "e_3", TaskID: "t_3", Worker: "box"}, {ExecID: "e_4", TaskID: "t_4", Worker: "other"}},
		traj: map[string]runs.Trajectory{"e_1": good, "e_2": unchecked},
	}
	var out bytes.Buffer
	sum, err := Write(context.Background(), &out, src, Options{Worker: "box"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if sum.Runs != 3 || sum.Exported != 1 || len(lines) != 1 {
		t.Fatalf("summary %+v, %d lines", sum, len(lines))
	}
	if sum.Skipped["not a checked done"] != 1 || sum.Skipped["its run record could not be built"] != 1 {
		t.Fatalf("skipped %v", sum.Skipped)
	}
	var rec Record
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil || rec.Schema != Schema || rec.RunID != "e_1" {
		t.Fatalf("line: %s %v", lines[0], err)
	}
}
