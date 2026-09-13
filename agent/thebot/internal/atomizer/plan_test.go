package atomizer

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"thebot/internal/llm"
)

func TestParsePlanRoutesTagsToSkills(t *testing.T) {
	steps := parsePlan(`1. [web] newest Go version release
2. [write] a short note about it to go-version.md (after 1)
3. [shell] ls -la ~/work
4. [do] make sure the note is readable by everyone
5. [docs] Go release notes
6. [download] the PDF files from https://example.com/reports
7. [code] a CLI that prints the Go version`, nil)

	want := []struct {
		typ, text string
	}{
		{"web", "search the web for newest Go version release"},
		{"file-write", "write a short note about it to go-version.md"},
		{"terminal", "run the command: ls -la ~/work"},
		{"operator", "make sure the note is readable by everyone"},
		{"rag-query", "search the indexed documents for Go release notes"},
		{"download", "download the PDF files from https://example.com/reports"},
		{"coding", "a CLI that prints the Go version"},
	}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d: %+v", len(steps), len(want), steps)
	}
	for i, w := range want {
		if steps[i].Type != w.typ || steps[i].Text != w.text {
			t.Errorf("step %d = [%s] %q, want [%s] %q", i+1, steps[i].Type, steps[i].Text, w.typ, w.text)
		}
		if steps[i].ID != i+1 {
			t.Errorf("step %d has ID %d", i+1, steps[i].ID)
		}
	}
}

// A small model tags an English goal as [shell] often enough to matter.
// Running "check how much disk space is free" in bash executes a sentence.
func TestParsePlanTurnsProseShellStepsIntoGoals(t *testing.T) {
	steps := parsePlan("1. [shell] check how much disk space is free on /", nil)
	if len(steps) != 1 || steps[0].Type != "operator" {
		t.Fatalf("steps = %+v, want one operator step", steps)
	}
	steps = parsePlan("1. [shell] df -h /", nil)
	if len(steps) != 1 || steps[0].Type != "terminal" {
		t.Fatalf("steps = %+v, want one terminal step", steps)
	}
}

// Sequential by default: a model that forgets "(after 1)" must not have
// its write run before the search it depends on. Consecutive research
// steps are the exception - they cannot affect each other.
func TestParsePlanWiresDependencies(t *testing.T) {
	steps := parsePlan(`1. [web] Blossom algorithm
2. [web] Li Chao tree
3. [web] centroid decomposition
4. [write] a summary of all three to algorithms.md
5. [shell] wc -l algorithms.md`, nil)

	needs := [][]int{nil, nil, nil, {1, 2, 3}, {4}}
	for i, want := range needs {
		if !reflect.DeepEqual(steps[i].Needs, want) {
			t.Errorf("step %d needs %v, want %v", i+1, steps[i].Needs, want)
		}
	}
}

func TestParsePlanHonoursStatedDependencies(t *testing.T) {
	steps := parsePlan(`1. [web] Go release notes
2. [shell] go version
3. [write] a comparison of the installed and newest Go to go.md (after 1, 2)`, nil)
	if !reflect.DeepEqual(steps[2].Needs, []int{1, 2}) {
		t.Errorf("step 3 needs %v, want [1 2]", steps[2].Needs)
	}
	if steps[2].Text != "write a comparison of the installed and newest Go to go.md" {
		t.Errorf("the dependency annotation leaked into the step text: %q", steps[2].Text)
	}
}

// A dropped step must not leave a dangling dependency or a hole in the IDs.
func TestParsePlanRenumbersAroundDroppedSteps(t *testing.T) {
	steps := parsePlan(`1. [web] newest Go version
2. Save the file.
3. [write] a note to go.md (after 1)
4. [answer] report the results back to the user`, nil)
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2 (a no-op and a trailing report dropped): %+v", len(steps), steps)
	}
	if steps[1].ID != 2 || !reflect.DeepEqual(steps[1].Needs, []int{1}) {
		t.Errorf("step 2 = %+v, want ID 2 needing step 1", steps[1])
	}
}

func TestParsePlanAcceptsTheOldUntaggedFormat(t *testing.T) {
	steps := parsePlan("TASK: yes\nSTEPS:\n1. search the web for X\n2. run the command: echo hi", nil)
	if len(steps) != 2 || steps[0].Type != "web" || steps[1].Type != "terminal" {
		t.Errorf("steps = %+v, want web then terminal", steps)
	}
}

type recordingProvider struct {
	response string
	messages []llm.Message
}

func (r *recordingProvider) Name() string                         { return "recording" }
func (r *recordingProvider) IsReachable(ctx context.Context) bool { return true }
func (r *recordingProvider) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	r.messages = messages
	return llm.Result{Text: r.response}, nil
}

// Planning calls open with the job's shared system frame so they reuse the
// prompt prefix the rest of the job already paid for.
func TestPlannerOpensWithTheSharedSystemFrame(t *testing.T) {
	rec := &recordingProvider{response: "1. [web] a\n2. [write] b to b.md"}
	frame := llm.Message{Role: "system", Content: "shared frame"}
	Planner{Provider: rec, Model: "m", System: &frame}.Atomize(context.Background(), "research X and then write a summary to b.md")

	if len(rec.messages) != 2 || rec.messages[0] != frame {
		t.Fatalf("messages = %+v, want the shared frame first", rec.messages)
	}
}

func TestReplanCanAskTheHuman(t *testing.T) {
	rec := &recordingProvider{response: "ASK: which of the two backup drives should I use?"}
	plan := Planner{Provider: rec, Model: "m"}.Replan(context.Background(), "back up ~/work", "1. [do] back up ~/work - FAILED: two drives mounted")
	if plan.Question != "which of the two backup drives should I use?" || len(plan.Steps) != 0 {
		t.Errorf("plan = %+v, want the question and no steps", plan)
	}
	if !strings.Contains(rec.messages[0].Content, "FAILED: two drives mounted") {
		t.Error("the failure history never reached the model")
	}
}

func TestDecomposeAlwaysPlans(t *testing.T) {
	rec := &recordingProvider{response: "1. [shell] python3 -m venv /tmp/v\n2. [shell] /tmp/v/bin/pip install requests"}
	plan := Planner{Provider: rec, Model: "m"}.Decompose(context.Background(), "create a venv then install requests into it")
	if len(plan.Steps) != 2 || plan.Steps[1].Type != "terminal" {
		t.Errorf("plan = %+v, want two terminal steps", plan)
	}
}

// Measured: the planner wrote its own guess into the write step behind a
// search, and file-write saved the guess instead of what the search found.
func TestScrubInventedContentAfterResearch(t *testing.T) {
	steps := parsePlan(`1. [web] newest Go version release
2. [write] "The newest Go version is v1.20.5" to go-version.md (after 1)`, nil)
	scrubInventedContent(steps, "find out the newest Go version and save a short note about it to go-version.md", nil)
	if strings.Contains(steps[1].Text, "1.20.5") || !strings.Contains(steps[1].Text, "go-version.md") {
		t.Errorf("step 2 = %q, want the invented content replaced and the filename kept", steps[1].Text)
	}
}

// The human's own words are never scrubbed.
func TestScrubInventedContentKeepsTheHumansOwnText(t *testing.T) {
	request := `search the web for today's weather and then write "remember your umbrella" to notes.txt`
	steps := parsePlan(`1. [web] today's weather
2. [write] "remember your umbrella" to notes.txt (after 1)`, nil)
	scrubInventedContent(steps, request, nil)
	if !strings.Contains(steps[1].Text, "remember your umbrella") {
		t.Errorf("step 2 = %q, want the human's literal kept", steps[1].Text)
	}
}

// Measured: the planner's last step became a shell grep of a file that did
// not exist, nothing wrote the note, and the job reported success anyway.
func TestEnsureCoverageAddsTheMissingWrite(t *testing.T) {
	request := "find out the newest Go version and save a short note about it to go-version.md"
	steps := parsePlan(`1. [web] newest Go version release
2. [web] https://golang.org/dl/ (after 1)
3. [do] grep "go" -A 5 index.html | tail -n 1 > go-version.md (after 2)`, nil)
	steps = ensureCoverage(steps, request, Triage(request), nil)

	last := steps[len(steps)-1]
	if len(steps) != 3 || last.Type != "file-write" || last.Text != request {
		t.Fatalf("steps = %+v, want the grep replaced by the request as a file-write", steps)
	}
	if !reflect.DeepEqual(last.Needs, []int{1, 2}) {
		t.Errorf("the write needs %v, want both research steps", last.Needs)
	}
}

func TestEnsureCoverageLeavesAGoodPlanAlone(t *testing.T) {
	request := "find out the newest Go version and save a short note about it to go-version.md"
	steps := parsePlan("1. [web] newest Go version\n2. [write] a short note about the newest Go version to go-version.md (after 1)", nil)
	got := ensureCoverage(append([]Step(nil), steps...), request, Triage(request), nil)
	if !reflect.DeepEqual(got, steps) {
		t.Errorf("plan changed: %+v", got)
	}
}

// Measured: a shell pipeline tagged [code] would have started a full build.
func TestParsePlanTurnsCommandsTaggedAsCodeIntoCommands(t *testing.T) {
	steps := parsePlan(`1. [code] grep -oP '(?<=go-)\d+\.\d+\.\d+' index.html | sort -nr | head -n 1`, nil)
	if len(steps) != 1 || steps[0].Type != "terminal" {
		t.Fatalf("steps = %+v, want one terminal step", steps)
	}
	steps = parsePlan("1. [code] a CLI in Go that prints the newest Go version", nil)
	if steps[0].Type != "coding" {
		t.Errorf("a real build request became %s", steps[0].Type)
	}
}
