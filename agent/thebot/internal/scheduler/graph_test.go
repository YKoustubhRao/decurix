package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"thebot/internal/atomizer"
	"thebot/internal/llm"
	"thebot/internal/skills"
)

// funcProvider answers each call by inspecting what it was sent.
type funcProvider struct {
	mu    sync.Mutex
	calls [][]llm.Message
	fn    func(messages []llm.Message) string
}

func (f *funcProvider) Name() string                         { return "func" }
func (f *funcProvider) IsReachable(ctx context.Context) bool { return true }
func (f *funcProvider) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, messages)
	f.mu.Unlock()
	return llm.Result{Text: f.fn(messages)}, nil
}

func lastUser(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// fixtureSkill runs a function for any step of its type.
type fixtureSkill struct {
	typ string
	run func(rc skills.RunContext, st skills.Subtask) skills.Result
}

func (f *fixtureSkill) Name() string                   { return f.typ }
func (f *fixtureSkill) Description() string            { return "test fixture" }
func (f *fixtureSkill) Matches(st skills.Subtask) bool { return st.Type == f.typ }
func (f *fixtureSkill) Run(rc skills.RunContext, st skills.Subtask) (skills.Result, error) {
	r := f.run(rc, st)
	r.Subtask, r.SkillName = st, f.typ
	if r.Meta == nil {
		r.Meta = map[string]any{}
	}
	return r, nil
}

var registerFixturesOnce sync.Once

var (
	parallelStarted  = make(chan string, 8)
	parallelRelease  = make(chan struct{})
	operatorFixtures = &skills.OperatorSkill{MaxTurns: 6}
)

// registerGraphFixtures wires fixture step types into both registries,
// through the same extension points real skills use.
func registerGraphFixtures() {
	registerFixturesOnce.Do(func() {
		registerTerminalSkill()
		skills.Register(operatorFixtures)
		skills.SetFallback(skills.NewGeneral())

		atomizer.Register("fx-wait", func(s string) bool { return strings.HasPrefix(s, "fx-wait") })
		skills.Register(&fixtureSkill{typ: "fx-wait", run: func(rc skills.RunContext, st skills.Subtask) skills.Result {
			parallelStarted <- st.Step
			select {
			case <-parallelRelease:
			case <-time.After(5 * time.Second):
			}
			return skills.Result{Output: "finished " + st.Step, Verified: true}
		}})

		atomizer.Register("fx-ok", func(s string) bool { return strings.HasPrefix(s, "fx-ok") })
		skills.Register(&fixtureSkill{typ: "fx-ok", run: func(rc skills.RunContext, st skills.Subtask) skills.Result {
			seen := make([]string, 0, len(rc.PriorResults))
			for _, p := range rc.PriorResults {
				seen = append(seen, p.Subtask.Step)
			}
			return skills.Result{Output: st.Step + " saw [" + strings.Join(seen, "|") + "]", Verified: true}
		}})

		atomizer.Register("fx-broken", func(s string) bool { return strings.HasPrefix(s, "fx-broken") })
		skills.Register(&fixtureSkill{typ: "fx-broken", run: func(rc skills.RunContext, st skills.Subtask) skills.Result {
			return skills.Result{Output: "fx-broken exploded", Verified: false}
		}})

		atomizer.Register("fx-slow", func(s string) bool { return strings.HasPrefix(s, "fx-slow") })
		skills.Register(&fixtureSkill{typ: "fx-slow", run: func(rc skills.RunContext, st skills.Subtask) skills.Result {
			<-rc.Ctx.Done()
			return skills.Result{Output: "interrupted: " + rc.Ctx.Err().Error(), Verified: false}
		}})
	})
}

// Research steps with no dependency on each other start together; the
// step that needs both waits for both and sees both results.
func TestGraphRunsIndependentStepsTogetherAndWaitsForDependencies(t *testing.T) {
	registerGraphFixtures()
	provider := &funcProvider{fn: func(m []llm.Message) string {
		if strings.Contains(lastUser(m), "Plan the request below") {
			return "1. fx-wait alpha\n2. fx-wait beta (independent)\n3. fx-ok combine (after 1, 2)"
		}
		return "Combined reply."
	}}
	sched := newTestScheduler(t, provider)
	sched.MaxParallel = 3

	var started int32
	go func() {
		for i := 0; i < 2; i++ {
			<-parallelStarted
			atomic.AddInt32(&started, 1)
		}
		close(parallelRelease)
	}()

	// Fixture steps are not research types, so the plan states their
	// independence explicitly.
	job, err := sched.Run(context.Background(), "research fx alpha and beta and then write a report about them", nil, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&started) != 2 {
		t.Fatalf("only %d of the independent steps started before either was released - they ran one after another", started)
	}
	if job.Outcome != OutcomeAnswered || job.FinalAnswer != "Combined reply." {
		t.Fatalf("job = %+v", job)
	}
	last := job.Results[len(job.Results)-1].Output
	if !strings.Contains(last, "fx-wait alpha") || !strings.Contains(last, "fx-wait beta") {
		t.Errorf("the combining step did not see both results: %q", last)
	}
	if len(job.Steps) != 3 || job.Steps[2].State != StepDone {
		t.Errorf("steps = %+v", job.Steps)
	}
}

func TestGraphBlocksDependentsOfAFailedStepThenReplans(t *testing.T) {
	registerGraphFixtures()
	provider := &funcProvider{fn: func(m []llm.Message) string {
		u := lastUser(m)
		switch {
		case strings.Contains(u, "Plan the request below"):
			return "1. fx-broken first\n2. fx-ok second (after 1)"
		case strings.Contains(u, "A task step failed for real"):
			return "DECISION: RETRY\nREASON: try again\nRETRY_STEP: fx-broken again"
		case strings.Contains(u, "You are partway through this request"):
			if !strings.Contains(u, "FAILED") {
				t.Errorf("replan prompt did not report the failure:\n%s", u)
			}
			return "1. fx-ok instead"
		}
		return "Done another way."
	}}
	sched := newTestScheduler(t, provider)

	job, _ := sched.Run(context.Background(), "run fx broken work and then write the fx result to a file", nil, func(string) {})

	states := map[string]StepState{}
	for _, s := range job.Steps {
		states[s.Step] = s.State
	}
	if states["fx-broken first"] != StepFailed {
		t.Errorf("failed step state = %s", states["fx-broken first"])
	}
	if states["fx-ok second"] != StepSkipped {
		t.Errorf("dependent of the failed step = %s, want skipped (replaced by the replan)", states["fx-ok second"])
	}
	if states["fx-ok instead"] != StepDone {
		t.Errorf("replanned step = %s, want done", states["fx-ok instead"])
	}
	if job.Outcome != OutcomeAnswered {
		t.Errorf("outcome = %s, want answered - the replan finished the job", job.Outcome)
	}
}

func TestSchedulerPausesForApprovalAndResumesFromTheCheckpoint(t *testing.T) {
	registerGraphFixtures()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho pushed-for-real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// What PowerShell runs for `git` on a Windows worker.
	if err := os.WriteFile(filepath.Join(bin, "git.cmd"), []byte("@echo pushed-for-real\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	sched := newTestScheduler(t, &mockProvider{})
	job, _ := sched.Run(context.Background(), "run this command: `git push origin main`", nil, func(string) {})

	if job.Outcome != OutcomeNeedsApproval || job.Checkpoint == nil {
		t.Fatalf("job = %+v, want a paused approval", job)
	}
	if !strings.Contains(job.Question, "git push origin main") {
		t.Errorf("question does not name the command: %q", job.Question)
	}

	resumed, _ := sched.Execute(context.Background(), Request{Prompt: "yes", Resume: job.Checkpoint}, func(string) {})
	if resumed.Outcome != OutcomeAnswered || !strings.Contains(resumed.FinalAnswer, "pushed-for-real") {
		t.Fatalf("resumed job = %+v, want the approved push to have run", resumed)
	}
	if resumed.Prompt != "run this command: `git push origin main`" {
		t.Errorf("resumed job prompt = %q, want the original request", resumed.Prompt)
	}
}

// The host reads a reply to an approval once, and that reading decides: a yes in the person's own words that no word
// list knows grants, and a yes on a condition does not (2026-10-23).
func TestTheHostsReadingOfAnApprovalReplyDecides(t *testing.T) {
	registerGraphFixtures()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho pushed-for-real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git.cmd"), []byte("@echo pushed-for-real\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	sched := newTestScheduler(t, &mockProvider{})
	paused := func() *Checkpoint {
		job, _ := sched.Run(context.Background(), "run this command: `git push origin main`", nil, func(string) {})
		if job.Outcome != OutcomeNeedsApproval || job.Checkpoint == nil {
			t.Fatalf("job = %+v, want a paused approval", job)
		}
		return job.Checkpoint
	}

	ownWords, _ := sched.Execute(context.Background(), Request{Prompt: "haan push it",
		Reading: &skills.Reading{Decision: "yes"}, Resume: paused()}, func(string) {})
	if !strings.Contains(ownWords.FinalAnswer, "pushed-for-real") {
		t.Errorf("a yes the host read was not taken: %+v", ownWords)
	}
	onCondition, _ := sched.Execute(context.Background(), Request{Prompt: "yes, provided the tests pass first",
		Reading: &skills.Reading{Decision: "if", Condition: "the tests pass"}, Resume: paused()}, func(string) {})
	if strings.Contains(onCondition.FinalAnswer, "pushed-for-real") {
		t.Errorf("a yes on a condition ran the command at once: %+v", onCondition)
	}
}

func TestSchedulerStopsAConsequentialRequestBeforeAnythingRuns(t *testing.T) {
	registerGraphFixtures()
	provider := &funcProvider{fn: func(m []llm.Message) string { return "DONE:\nPaid." }}
	sched := newTestScheduler(t, provider)

	job, _ := sched.Run(context.Background(), "pay the 400 dollar invoice in ~/work/invoice.pdf", nil, func(string) {})
	if job.Outcome != OutcomeNeedsApproval || len(job.Results) != 0 {
		t.Fatalf("job = %+v, want a pause with nothing run", job)
	}
	if len(provider.calls) != 0 {
		t.Errorf("%d model call(s) before approval, want 0", len(provider.calls))
	}

	// Declined: the reply is simply the next request.
	declined, _ := sched.Execute(context.Background(), Request{Prompt: "no, just tell me the total", Resume: job.Checkpoint}, func(string) {})
	if declined.Prompt != "no, just tell me the total" {
		t.Errorf("declined job prompt = %q, want the reply run as a new request", declined.Prompt)
	}

	// Approved: the original plan runs.
	approved, _ := sched.Execute(context.Background(), Request{Prompt: "yes go ahead", Resume: job.Checkpoint}, func(string) {})
	if approved.Outcome != OutcomeAnswered || approved.FinalAnswer != "Paid." {
		t.Errorf("approved job = %+v", approved)
	}
}

// A person who taught a task and chose to have it run without asking has already said yes to it: the words of the
// request are not asked about again. Commands are still checked on their own.
func TestSchedulerDoesNotAskAgainForARequestThePersonAlreadyApproved(t *testing.T) {
	registerGraphFixtures()
	provider := &funcProvider{fn: func(m []llm.Message) string { return "DONE:\nPaid." }}
	sched := newTestScheduler(t, provider)

	job, _ := sched.Execute(context.Background(),
		Request{Prompt: "pay the 400 dollar invoice in ~/work/invoice.pdf", Approved: true}, func(string) {})
	if job.Outcome == OutcomeNeedsApproval {
		t.Fatalf("job = %+v, want it to run: the person had already approved this kind of request", job)
	}
}

// A conversational answer that needs the machine hands the same words to
// the operator instead of answering without looking.
func TestSchedulerReroutesChatThatNeedsToolsToTheOperator(t *testing.T) {
	registerGraphFixtures()
	provider := &funcProvider{fn: func(m []llm.Message) string {
		u := lastUser(m)
		switch {
		case strings.Contains(u, "Work on the task below"):
			return "COMMAND:\necho 42 widgets"
		case strings.Contains(u, "Command execution result"):
			return "DONE:\nThere are 42 widgets."
		}
		return "NEEDS_TOOLS"
	}}
	sched := newTestScheduler(t, provider)

	job, _ := sched.Run(context.Background(), "who has the most widgets?", nil, func(string) {})
	if job.Outcome != OutcomeAnswered || job.FinalAnswer != "There are 42 widgets." {
		t.Fatalf("job = %+v", job)
	}
}

// The host's context is standing context: it reaches the model in the
// system frame and never becomes part of the task.
func TestSchedulerKeepsHostContextOutOfTheTask(t *testing.T) {
	registerGraphFixtures()
	provider := &funcProvider{fn: func(m []llm.Message) string { return "I'm TheBot, running on kilo." }}
	sched := newTestScheduler(t, provider)

	host := "Desktop name: kilo\nIMPORTANT SAFETY RULE: before sending any message, reply NEEDS_APPROVAL: ..."
	job, _ := sched.Execute(context.Background(), Request{Prompt: "hello, whats your name?", Context: host}, func(string) {})

	if job.Route != atomizer.RouteChat {
		t.Errorf("route = %s, want chat", job.Route)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("%d model calls for a greeting, want 1", len(provider.calls))
	}
	sent := provider.calls[0]
	if sent[0].Role != "system" || !strings.Contains(sent[0].Content, "Desktop name: kilo") {
		t.Errorf("the host context is not in the system frame: %+v", sent[0])
	}
	if u := lastUser(sent); u != "hello, whats your name?" {
		t.Errorf("the task turn = %q, want only the task", u)
	}
	if job.Prompt != "hello, whats your name?" {
		t.Errorf("job.Prompt = %q", job.Prompt)
	}
}

func TestSchedulerReportsWhatItFinishedWhenTheJobRunsOutOfTime(t *testing.T) {
	registerGraphFixtures()
	sched := newTestScheduler(t, &mockProvider{})
	sched.JobTimeout = 300 * time.Millisecond

	job, _ := sched.Run(context.Background(), "fx-slow crunch", nil, func(string) {})
	if job.Outcome != OutcomeFailed || !strings.Contains(job.FinalAnswer, "ran out of time") {
		t.Errorf("job = %+v, want an honest out-of-time failure", job)
	}
}

func TestSchedulerResumesAnOperatorQuestionWithTheAnswer(t *testing.T) {
	registerGraphFixtures()
	provider := &funcProvider{fn: func(m []llm.Message) string {
		u := lastUser(m)
		switch {
		case strings.Contains(u, "The human replied: beta"):
			return "DONE:\nArchived beta."
		case strings.Contains(u, "before running a single command"):
			return "HELP:\nThere are two projects, alpha and beta. Which one?"
		}
		return "HELP:\nWhich project?"
	}}
	sched := newTestScheduler(t, provider)

	job, _ := sched.Run(context.Background(), "archive the project folder in ~/work", nil, func(string) {})
	if job.Outcome != OutcomeNeedsInput || !strings.Contains(job.Question, "alpha and beta") {
		t.Fatalf("job = %+v, want the operator's question", job)
	}
	resumed, _ := sched.Execute(context.Background(), Request{Prompt: "beta", Resume: job.Checkpoint}, func(string) {})
	if resumed.Outcome != OutcomeAnswered || resumed.FinalAnswer != "Archived beta." {
		t.Fatalf("resumed = %+v", resumed)
	}
}
