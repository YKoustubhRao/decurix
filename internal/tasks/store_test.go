package tasks

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// clock is a settable time source, so durations and windows are exact.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)} }
func ptrFloat(value float64) *float64    { return &value }
func ptrInt(value int64) *int64          { return &value }
func worker(id, engine string) Worker {
	return Worker{ID: id, Name: id, Engine: engine, Model: "qwen2.5-coder:3b"}
}
func openStore(t *testing.T, c *clock) *Store { return openStoreAt(t, c, "") }

func openStoreAt(t *testing.T, c *clock, path string) *Store {
	t.Helper()

	var err error
	var store *Store

	db, err := sqlitedb.OpenMemory()
	if path != "" {
		db, err = sqlitedb.Open(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err = NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	store.now = c.now

	return store
}

// send is what httpapi.sendRequestHandler does: plan, deliver, commit.
func send(t *testing.T, s *Store, w Worker, text string) Route {
	t.Helper()

	route, err := s.Plan(w.ID, text)
	if err != nil {
		t.Fatal(err)
	}

	route, err = s.Commit(route, w, text)
	if err != nil {
		t.Fatal(err)
	}

	return route
}

func outcome(t *testing.T, s *Store, workerID, taskID string, o Outcome) string {
	t.Helper()

	got, err := s.RecordOutcome(workerID, taskID, o)
	if err != nil {
		t.Fatal(err)
	}

	return got
}

func get(t *testing.T, s *Store, taskID string) Detail {
	t.Helper()

	detail, err := s.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}

	return detail
}

func TestAWholeTaskIsRecorded(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "ollama")

	route := send(t, s, box, "what is the kernel version?")

	if route.Kind != RouteNew || !ValidID(route.TaskID) {
		t.Fatalf("route = %+v", route)
	}

	c.advance(2 * time.Second)

	if ok, err := s.AddStep("box", route.TaskID, Step{Text: "$ uname -r"}); !ok || err != nil {
		t.Fatalf("AddStep = %v, %v", ok, err)
	}

	c.advance(40 * time.Second)

	if got := outcome(t, s, "box", route.TaskID, Outcome{
		Kind: OutcomeDone, Reply: "6.6.87.2", Engine: "ollama", Model: "qwen2.5-coder:3b",
		CostUSD: ptrFloat(0), InputTokens: ptrInt(900), OutputTokens: ptrInt(40),
	}); got != route.TaskID {
		t.Fatalf("outcome attributed to %q, want %q", got, route.TaskID)
	}

	detail := get(t, s, route.TaskID)

	if detail.Status != StatusDone || detail.Type != TypeSystem || len(detail.Executions) != 1 {
		t.Fatalf("task = %+v", detail.Task)
	}

	execution := detail.Executions[0]

	if execution.FinalReply != "6.6.87.2" || len(execution.Steps) != 1 || *execution.InputTokens != 900 {
		t.Fatalf("execution = %+v", execution)
	}

	if got := execution.EndedAt.Sub(execution.StartedAt); got != 42*time.Second {
		t.Fatalf("time taken = %v, want 42s", got)
	}
}

// A reply to a paused task is not a new task: it is recorded as the human's
// answer to that intervention, and the same execution carries on.
func TestReplyResumesTheInterventionItAnswers(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "claude")

	route := send(t, s, box, "pay the 400 dollar invoice")
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeApproval, Reply: "Approval needed before I click Pay now: $400 to ACME."})

	c.advance(3 * time.Minute)

	reply := send(t, s, box, "yes")

	if reply.Kind != RouteReply || reply.TaskID != route.TaskID {
		t.Fatalf("reply routed as %+v, want a reply to %s", reply, route.TaskID)
	}

	if got := get(t, s, route.TaskID).Status; got != StatusRunning {
		t.Fatalf("status after the reply = %s, want running", got)
	}

	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "Paid."})

	detail := get(t, s, route.TaskID)
	interventions := detail.Executions[0].Interventions

	if len(detail.Executions) != 1 || len(interventions) != 1 {
		t.Fatalf("got %d executions, %d interventions", len(detail.Executions), len(interventions))
	}

	if got := interventions[0]; got.Reply != "yes" || got.Kind != InterventionApproval || got.Reason != ReasonApprovalPayment {
		t.Fatalf("intervention = %+v", got)
	}

	if waited := interventions[0].RepliedAt.Sub(interventions[0].RequestedAt); waited != 3*time.Minute {
		t.Fatalf("waited %v, want 3m", waited)
	}
}

// A reply planned against one open intervention must not land on a
// different one that has since opened, if its delivery is slow enough that
// the execution paused again before the Commit for it finally runs.
func TestSlowReplyAttachesToTheInterventionOpenAtCommitTime(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "claude")

	route := send(t, s, box, "pick a color to paint the wall")
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeQuery, Reply: "which color?"})

	// Human A starts replying, but delivery of A's reply stalls - only
	// Plan has run so far, nothing is committed yet.
	routeA, err := s.Plan(box.ID, "blue")
	if err != nil {
		t.Fatal(err)
	}

	c.advance(time.Second)

	// Human B's faster reply reaches Commit first, resuming the execution.
	if reply := send(t, s, box, "blue-ish"); reply.Kind != RouteReply {
		t.Fatalf("B's reply routed as %+v, want a reply", reply)
	}

	// The agent resumes on B's answer and pauses again, on a new
	// intervention.
	c.advance(time.Second)
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeQuery, Reply: "light or dark?"})

	// A's stale route finally reaches Commit.
	if _, err := s.Commit(routeA, box, "blue"); err != nil {
		t.Fatal(err)
	}

	interventions := get(t, s, route.TaskID).Executions[0].Interventions
	if len(interventions) != 2 {
		t.Fatalf("got %d interventions, want 2", len(interventions))
	}

	first, second := interventions[0], interventions[1]

	if first.Reply != "blue-ish" {
		t.Fatalf("first intervention's reply = %q, want it untouched by A's stale reply", first.Reply)
	}

	if second.Reply != "blue" || second.RepliedAt.IsZero() {
		t.Fatalf("second intervention = %+v, want A's stale reply attached to it, answered", second)
	}
}

// The failure note says "send the request again"; doing so is another
// attempt at the same task, not a second task.
func TestResendingAfterAnErrorIsARetry(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "thebot")

	first := send(t, s, box, "hello, whats your name?")
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeError, Reply: "did not finish answering in time"})

	retry := send(t, s, box, "Hello,  whats your name?")

	if retry.Kind != RouteRetry || retry.TaskID != first.TaskID {
		t.Fatalf("resend routed as %+v", retry)
	}

	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeDone, Reply: "I'm box."})

	detail := get(t, s, first.TaskID)

	if len(detail.Executions) != 2 || detail.Executions[1].Attempt != 2 || detail.Status != StatusDone {
		t.Fatalf("detail = %+v", detail)
	}

	summaries, _ := s.List(Filter{})

	if summaries[0].Autonomous {
		t.Fatal("a task that needed a resend is not autonomous")
	}
}

// A desktop built before task IDs reports none; its steps and outcome still
// land on the task it is running.
func TestReportsWithoutATaskIDFallBackToTheLiveTask(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "ollama")

	route := send(t, s, box, "tidy up /work")

	if ok, _ := s.AddStep("box", "", Step{Text: "$ ls /work"}); !ok {
		t.Fatal("step without a task id was dropped")
	}

	if got := outcome(t, s, "box", "", Outcome{Kind: OutcomeDone, Reply: "Tidy."}); got != route.TaskID {
		t.Fatalf("outcome attributed to %q, want %q", got, route.TaskID)
	}

	// Its last line arrives just after the reply.
	c.advance(time.Second)
	if ok, _ := s.AddStep("box", "", Step{Text: "wrote tidy.log"}); !ok {
		t.Fatal("a step trailing the reply was dropped")
	}

	// Work nobody asked for, long after, is not a task.
	c.advance(time.Hour)
	if ok, _ := s.AddStep("box", route.TaskID, Step{Text: "gui observe"}); ok {
		t.Fatal("a step with no live task was attached to an old one")
	}

	if got := len(get(t, s, route.TaskID).Executions[0].Steps); got != 2 {
		t.Fatalf("got %d steps, want 2", got)
	}
}

// A desktop runs one task at a time and holds the next request; the held one
// starts, from its own start time, when the first ends.
func TestARequestSentWhileBusyIsQueued(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "ollama")

	first := send(t, s, box, "count the files in /etc")
	c.advance(time.Second)
	second := send(t, s, box, "what is the hostname?")

	if got := get(t, s, second.TaskID).Status; got != StatusQueued {
		t.Fatalf("second task is %s, want queued", got)
	}

	c.advance(30 * time.Second)
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeDone, Reply: "241"})

	c.advance(10 * time.Second)
	outcome(t, s, "box", second.TaskID, Outcome{Kind: OutcomeDone, Reply: "box"})

	execution := get(t, s, second.TaskID).Executions[0]

	if execution.Status != StatusDone || execution.EndedAt.Sub(execution.StartedAt) != 10*time.Second {
		t.Fatalf("queued execution = %+v", execution)
	}
}

// A separately queued task must survive a later question on the active task.
func TestAHeldRequestRemainsQueuedWhenTheTaskPauses(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "ollama")

	first := send(t, s, box, "email my landlord")
	held := send(t, s, box, "use the gmail account")

	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeQuery, Reply: "Should I send this email to the landlord?"})

	if got := get(t, s, held.TaskID).Status; got != StatusQueued {
		t.Fatalf("held task is %s, want queued", got)
	}

	detail := get(t, s, first.TaskID)

	if detail.Status != StatusWaiting || detail.Executions[0].Interventions[0].Reply != "" {
		t.Fatalf("detail = %+v, interventions = %+v", detail.Task, detail.Executions[0].Interventions)
	}
	answer := send(t, s, box, "yes")
	if answer.Kind != RouteReply || answer.TaskID != first.TaskID {
		t.Fatalf("answer routed as %+v", answer)
	}
	if get(t, s, held.TaskID).Status != StatusQueued {
		t.Fatal("answer consumed the queued task")
	}
}

func TestStoppingADesktopClosesItsLiveTasks(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "ollama")

	running := send(t, s, box, "build the project")

	other := worker("other", "claude")
	waiting := send(t, s, other, "buy a domain")
	outcome(t, s, "other", waiting.TaskID, Outcome{Kind: OutcomeApproval, Reply: "Approval needed before I buy example.com for $12"})

	if err := s.CloseOpen("box", "The desktop was restarted."); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseOpen("other", "The desktop was stopped."); err != nil {
		t.Fatal(err)
	}

	if got := get(t, s, running.TaskID).Status; got != StatusAbandoned {
		t.Fatalf("running task is %s, want abandoned", got)
	}

	if got := get(t, s, waiting.TaskID).Status; got != StatusQuery {
		t.Fatalf("waiting task is %s, want query", got)
	}

	// The paused agent is gone, so the next request starts over.
	if route := send(t, s, other, "yes"); route.Kind != RouteNew {
		t.Fatalf("request after a stop routed as %s", route.Kind)
	}
}

func TestAnActionStepMakesItOnScreenWork(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "claude")

	route := send(t, s, box, "add Dana Scott as an editor")
	payment := send(t, s, box, "pay invoice INV-1042")

	s.AddStep("box", route.TaskID, Step{Text: "click link Add user", Action: &Action{Kind: "click", Outcome: "done", Before: "/b", After: "/a"}})
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "Added."})
	s.AddStep("box", payment.TaskID, Step{Text: "click button Pay now", Action: &Action{Kind: "click", Outcome: "needs_approval"}})

	if got := get(t, s, route.TaskID); got.Type != TypeScreen || got.Executions[0].Steps[0].Action.After != "/a" {
		t.Fatalf("task = %+v", got)
	}

	if got := get(t, s, payment.TaskID).Type; got != TypeConsequential {
		t.Fatalf("a payment done on screen became %s", got)
	}
}

func TestVerdict(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "ollama")

	route := send(t, s, box, "how much disk is free?")
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "1.6T available"})

	task, err := s.SetVerdict(route.TaskID, VerdictWrong, "  df says 835G  ")
	if err != nil || task.Verdict != VerdictWrong || task.VerdictNote != "df says 835G" {
		t.Fatalf("SetVerdict = %+v, %v", task, err)
	}

	summaries, _ := s.List(Filter{})
	if summaries[0].Autonomous {
		t.Fatal("an answer marked wrong is not autonomous")
	}

	if task, _ := s.SetVerdict(route.TaskID, "", "ignored"); task.Verdict != "" || task.VerdictNote != "" {
		t.Fatalf("withdrawing the verdict left %+v", task)
	}

	if _, err := s.SetVerdict("t0000000000000000", VerdictCorrect, ""); err != ErrNotFound {
		t.Fatalf("unknown task: err = %v", err)
	}

	if _, err := s.SetVerdict(route.TaskID, "meh", ""); err == nil {
		t.Fatal("an unknown verdict was accepted")
	}
}

// "Done when a restart loses nothing": everything above, reopened from disk.
func TestRecordsSurviveRestart(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), sqlitedb.FileName)
	s := openStoreAt(t, c, path)
	box := worker("box", "claude")

	route := send(t, s, box, "send the weekly newsletter")
	s.AddStep("box", route.TaskID, Step{Text: "click Send", Action: &Action{Kind: "click", Outcome: "needs_approval", Before: "/b"}})
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeApproval, Reply: "Approval needed before I click Send newsletter", CostUSD: ptrFloat(0.12)})

	s.db.Close()

	reopened := openStoreAt(t, c, path)

	if reply := send(t, reopened, box, "yes"); reply.Kind != RouteReply || reply.TaskID != route.TaskID {
		t.Fatalf("after a restart the reply routed as %+v", reply)
	}

	outcome(t, reopened, "box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "Sent.", CostUSD: ptrFloat(0.31)})
	reopened.SetVerdict(route.TaskID, VerdictCorrect, "")

	detail := get(t, reopened, route.TaskID)
	execution := detail.Executions[0]

	if detail.Verdict != VerdictCorrect || len(execution.Steps) != 1 || len(execution.Interventions) != 1 ||
		execution.Interventions[0].Reply != "yes" || *execution.CostUSD != 0.31 || detail.Worker.Engine != "claude" {
		t.Fatalf("after restart: %+v / %+v", detail, execution)
	}
}

// The question the dashboard exists to answer: which engine had the best
// autonomy rate this week, and why did people have to step in?
func TestReportAnswersWhichEngineAndWhy(t *testing.T) {
	c := newClock()
	s := openStore(t, c)

	ollama := worker("ollama-box", "ollama")
	claude := worker("claude-box", "claude")

	// Last month: excluded from this week.
	old := send(t, s, claude, "what is the hostname?")
	outcome(t, s, "claude-box", old.TaskID, Outcome{Kind: OutcomeError, Reply: "claude exited with code 1"})

	c.advance(30 * 24 * time.Hour)
	weekStart := c.now().Add(-time.Minute)

	// Claude: three done, one of them after an approval.
	for _, text := range []string{"what is the kernel version?", "list files in /work"} {
		route := send(t, s, claude, text)
		c.advance(20 * time.Second)
		outcome(t, s, "claude-box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "ok", CostUSD: ptrFloat(0.05)})
	}

	paid := send(t, s, claude, "pay invoice INV-7")
	outcome(t, s, "claude-box", paid.TaskID, Outcome{Kind: OutcomeApproval, Reply: "Approval needed before I click Pay now: $40"})
	send(t, s, claude, "yes")
	outcome(t, s, "claude-box", paid.TaskID, Outcome{Kind: OutcomeDone, Reply: "Paid"})

	// Ollama: one done but wrong, one stuck, one timed out, one right.
	wrong := send(t, s, ollama, "how much disk is free?")
	outcome(t, s, "ollama-box", wrong.TaskID, Outcome{Kind: OutcomeDone, Reply: "1.6T"})
	s.SetVerdict(wrong.TaskID, VerdictWrong, "invented numbers")

	stuck := send(t, s, ollama, "sign in to the admin panel")
	outcome(t, s, "ollama-box", stuck.TaskID, Outcome{Kind: OutcomeQuery, Reply: "I need the admin password to sign in."})
	send(t, s, ollama, "it is in ~/secrets.txt")
	outcome(t, s, "ollama-box", stuck.TaskID, Outcome{Kind: OutcomeDone, Reply: "Signed in."})

	slow := send(t, s, ollama, "hello")
	outcome(t, s, "ollama-box", slow.TaskID, Outcome{Kind: OutcomeError, Reply: "Ollama accepted the request but did not finish answering in time. context deadline exceeded"})

	right := send(t, s, ollama, "uname -r")
	outcome(t, s, "ollama-box", right.TaskID, Outcome{Kind: OutcomeDone, Reply: "6.6.87.2"})
	s.SetVerdict(right.TaskID, VerdictCorrect, "")

	// A person recording themselves doing a task is not Ollama being autonomous,
	// even with a screen step attributed to it.
	demo := send(t, s, ollama, "/record fill in the invoice form")
	if _, err := s.AddStep("ollama-box", demo.TaskID, Step{Text: "click Save", Action: &Action{Kind: "click"}}); err != nil {
		t.Fatal(err)
	}
	outcome(t, s, "ollama-box", demo.TaskID, Outcome{Kind: OutcomeDone, Reply: "Recorded 1 step", Engine: "human"})

	if task, err := s.Get(demo.TaskID); err != nil || task.Type != TypeDemonstration {
		t.Fatalf("recorded task type = %q, %v", task.Type, err)
	}

	report, err := s.Report(weekStart)
	if err != nil {
		t.Fatal(err)
	}

	if report.Totals.Tasks != 7 || report.Totals.Settled != 7 {
		t.Fatalf("totals = %+v", report.Totals)
	}

	if report.Best == nil || report.Best.Key != "claude" || report.Best.Autonomous != 2 || report.Best.Settled != 3 {
		t.Fatalf("best = %+v", report.Best)
	}

	ollamaRow := report.ByEngine[1]
	if ollamaRow.Key != "ollama" || ollamaRow.RatePercent() != 25 || ollamaRow.Wrong != 1 || ollamaRow.Correct != 1 || ollamaRow.Errors != 1 {
		t.Fatalf("ollama row = %+v", ollamaRow)
	}

	if report.ByEngine[0].Cost() != "$0.10" {
		t.Fatalf("claude cost = %s", report.ByEngine[0].Cost())
	}

	reasons := map[string]int{}
	for _, reason := range report.Reasons {
		reasons[reason.Key] = reason.Count
	}

	want := map[string]int{ReasonApprovalPayment: 1, ReasonWrong: 1, ReasonCredentials: 1, ReasonBackend: 1}
	for key, count := range want {
		if reasons[key] != count {
			t.Errorf("reason %s: got %d, want %d (all: %v)", key, reasons[key], count, reasons)
		}
	}

	if len(reasons) != len(want) || report.StepIns != 4 {
		t.Fatalf("reasons = %v, step-ins = %d", reasons, report.StepIns)
	}

	if report.Reasons[0].Examples[0].TaskID == "" {
		t.Fatal("a reason came with no example task to open")
	}
}

func TestLastActivity(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	w := worker("bordu", "claude")

	if _, ok, err := s.LastActivity(w.ID); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a worker with no task records should report ok=false")
	}

	send(t, s, w, "hello")
	want := c.now()

	c.advance(time.Hour)

	got, ok, err := s.LastActivity(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a last-activity time once a task exists")
	}
	if !got.Equal(want) {
		t.Fatalf("last activity = %v, want %v", got, want)
	}
}

// internal/dag polls TaskResult to learn when a node it dispatched has
// finished - it must see the latest attempt, not the first one that failed.
func TestTaskResult(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "thebot")

	first := send(t, s, box, "hello, whats your name?")
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeError, Reply: "did not finish answering in time"})

	send(t, s, box, "Hello,  whats your name?")
	outcome(t, s, "box", first.TaskID, Outcome{Kind: OutcomeDone, Reply: "I'm box."})

	status, reply, ok := s.TaskResult(first.TaskID)
	if !ok {
		t.Fatal("TaskResult ok = false, want true")
	}

	if status != StatusDone || reply != "I'm box." {
		t.Fatalf("TaskResult = (%q, %q), want the latest attempt (%q, %q)", status, reply, StatusDone, "I'm box.")
	}
}

func TestTaskResultUnknownID(t *testing.T) {
	s := openStore(t, newClock())

	if _, _, ok := s.TaskResult("t0000000000000000"); ok {
		t.Fatal("TaskResult ok = true for an unknown task, want false")
	}
}

func TestAReportShowsWhatTasksCostInTokens(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	w := worker("box", "thebot")

	// The same kind of request three times: found out, then with what was kept from the first.
	for _, used := range [][2]int64{{180000, 2400}, {60000, 900}, {58000, 700}} {
		route := send(t, s, w, "enter the attached order")
		c.advance(time.Minute)
		outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "ok", InputTokens: ptrInt(used[0]), OutputTokens: ptrInt(used[1])})
	}

	// One that paused: each attempt's totals are added, as cost is.
	paused := send(t, s, w, "pay invoice INV-7")
	outcome(t, s, "box", paused.TaskID, Outcome{Kind: OutcomeError, Reply: "timed out", InputTokens: ptrInt(1000), OutputTokens: ptrInt(10)})
	send(t, s, w, "pay invoice INV-7")
	outcome(t, s, "box", paused.TaskID, Outcome{Kind: OutcomeDone, Reply: "Paid", InputTokens: ptrInt(2000), OutputTokens: ptrInt(20)})

	// And one whose engine reported nothing: unknown, not zero.
	silent := send(t, s, w, "uname -r")
	outcome(t, s, "box", silent.TaskID, Outcome{Kind: OutcomeDone, Reply: "6.6"})

	report, list, err := s.ReportAndList(Filter{})
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]Summary{}
	for _, sum := range list {
		byID[sum.ID] = sum
	}
	if got := byID[paused.TaskID].Tokens; got == nil || *got != 3030 {
		t.Fatalf("a task's tokens over two attempts = %v, want 3030", got)
	}
	if got := byID[silent.TaskID].Tokens; got != nil {
		t.Fatalf("a task nobody counted reports %d tokens, want none", *got)
	}

	if report.Totals.Tokens == nil || *report.Totals.Tokens != 182400+60900+58700+3030 || report.Totals.TokenTasks != 4 {
		t.Fatalf("totals = %v over %d tasks", report.Totals.Tokens, report.Totals.TokenTasks)
	}
	if got := report.Totals.TokensPerTask(); got != "76.3k" {
		t.Fatalf("tokens per task = %q, want 76.3k", got)
	}

	for want, n := range map[string]int64{"950": 950, "12.4k": 12400, "1.25M": 1250000} {
		if got := FormatTokens(&n); got != want {
			t.Fatalf("FormatTokens(%d) = %q, want %q", n, got, want)
		}
	}
	if FormatTokens(nil) != "—" {
		t.Fatal("no count must not read as a number")
	}
}
