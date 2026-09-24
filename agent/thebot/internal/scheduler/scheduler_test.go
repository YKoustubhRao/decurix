package scheduler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"thebot/internal/atomizer"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/skills"
	"thebot/internal/workspace"
)

// mockProvider is a real, deterministic fake - no network, no real model -
// so scheduler behavior (the failure-reasoning loop especially) can be
// tested without depending on a live Ollama server or being at the mercy
// of real model output variance.
type mockProvider struct {
	response string
	err      error
	calls    int
}

func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	m.calls++
	if m.err != nil {
		return llm.Result{}, m.err
	}
	return llm.Result{Text: m.response}, nil
}
func (m *mockProvider) IsReachable(ctx context.Context) bool { return m.err == nil }

// alwaysFailSkill is a real Skill that always reports a failure, so the
// failure-reasoning path can be exercised deterministically.
type alwaysFailSkill struct{ name string }

func (s *alwaysFailSkill) Name() string                   { return s.name }
func (s *alwaysFailSkill) Description() string            { return "test skill that always fails" }
func (s *alwaysFailSkill) Matches(st skills.Subtask) bool { return st.Type == "test-fail" }
func (s *alwaysFailSkill) Run(rc skills.RunContext, st skills.Subtask) (skills.Result, error) {
	return skills.Result{Subtask: st, SkillName: s.name, Output: "simulated real failure", Verified: false, Meta: map[string]any{}}, nil
}

var (
	registerTerminalOnce sync.Once
	registerTestFailOnce sync.Once
)

// registerTerminalSkill ensures the real terminal skill is registered
// exactly once across all tests in this package - skills.Register uses a
// package-level global registry, so redundant registration would just add
// harmless duplicates, but sync.Once keeps it clean.
func registerTerminalSkill() {
	registerTerminalOnce.Do(func() {
		skills.Register(skills.NewTerminal())
	})
}

// registerTestFailFixtures wires a synthetic "test-fail" task type into
// BOTH the atomizer's classifier chain and the skill registry - real
// registration through the real extension points every skill uses, not a
// shortcut around them.
func registerTestFailFixtures() {
	registerTestFailOnce.Do(func() {
		atomizer.Register("test-fail", func(step string) bool { return step == "trigger test-fail please" })
		skills.Register(&alwaysFailSkill{name: "always-fail"})
	})
}

func newTestScheduler(t *testing.T, provider llm.Provider) *Scheduler {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New() error = %v", err)
	}
	l2, err := memory.NewL2(ws.Root())
	if err != nil {
		t.Fatalf("memory.NewL2() error = %v", err)
	}
	// RAG/Feedback are nil here - these tests exercise the reasoning loop
	// itself, not RAG auto-ingestion or feedback consultation (both are
	// nil-safe on Scheduler and covered by their own dedicated tests
	// below, which need a real embedder).
	return New(provider, llm.DefaultModels("mock-model"), ws, l2, nil, nil, nil)
}

func TestSchedulerRunsSuccessfulTerminalTask(t *testing.T) {
	registerTerminalSkill()
	sched := newTestScheduler(t, &mockProvider{})

	var logs []string
	job, err := sched.Run(context.Background(), "run this command: `echo scheduler-test`", nil, func(s string) { logs = append(logs, s) })
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if job.Status != "done" {
		t.Errorf("Status = %q, want done", job.Status)
	}
	if len(job.Results) != 1 || !job.Results[0].Verified {
		t.Fatalf("expected 1 verified result, got %+v", job.Results)
	}
	if len(logs) == 0 {
		t.Error("no progress was logged")
	}
}

// The live bug this pins: "how much do you have indexed?" got "I don't
// have the ability to check" even though the real numbers were one query
// away the whole time. selfStats must surface real counts and never
// panic when a tier is unconfigured (the common case in tests, and for
// any deployment that hasn't wired up every tier).
func TestSelfStatsReturnsRealCountsAndIsNilSafe(t *testing.T) {
	sched := newTestScheduler(t, &mockProvider{}) // RAG/Feedback/L1 are nil here; L2 is real

	stats := sched.selfStats(context.Background())
	if stats == "" {
		t.Fatal("selfStats() = empty, want at least the L2 line")
	}
	if !strings.Contains(stats, "Job log (L2)") {
		t.Errorf("selfStats() = %q, want it to include the real L2 job count", stats)
	}
	if !strings.Contains(stats, "not configured") {
		t.Errorf("selfStats() = %q, want it to say RAG isn't configured rather than silently omitting it", stats)
	}
}

func TestSchedulerRetriesOnFailureWhenLLMSaysRetry(t *testing.T) {
	registerTestFailFixtures()

	mock := &mockProvider{response: "DECISION: RETRY\nREASON: try a different way\nRETRY_STEP: a different real approach"}
	sched := newTestScheduler(t, mock)

	job, err := sched.Run(context.Background(), "trigger test-fail please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if mock.calls != 1 {
		t.Errorf("expected exactly 1 LLM reasoning call, got %d", mock.calls)
	}
	result := job.Results[0]
	if result.Meta["retriedAfterFailure"] != true {
		t.Errorf("expected retriedAfterFailure=true in Meta, got %+v", result.Meta)
	}
	if result.Subtask.Step != "a different real approach" {
		t.Errorf("retry did not use the LLM's proposed step: got %q", result.Subtask.Step)
	}
}

// The live bug this pins: a rag-query step failed with "nothing indexed",
// the failure-reasoning model correctly proposed retrying as a real web
// search, and the retry still ran through the ORIGINAL failed step's type
// (rag-query) instead of the type the retry step actually describes - so
// it failed the exact same way again instead of ever reaching the web.
// The retry must be reclassified from what it actually says to do.
func TestSchedulerReclassifiesRetryStepInsteadOfInheritingOriginalType(t *testing.T) {
	registerTestFailFixtures()
	registerTerminalSkill()

	// The retry step is real terminal work, phrased so atomizer.Classify
	// resolves it to "terminal" - a completely different type than the
	// failed "test-fail" step it's retrying.
	mock := &mockProvider{response: "DECISION: RETRY\nREASON: try a real command instead\nRETRY_STEP: run this command: `echo reclassified`"}
	sched := newTestScheduler(t, mock)

	job, err := sched.Run(context.Background(), "trigger test-fail please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := job.Results[0]
	if result.SkillName != "terminal" {
		t.Fatalf("retry ran through skill %q, want terminal - the retry step's own content should have been reclassified, not inherited the original type", result.SkillName)
	}
	if !result.Verified {
		t.Fatalf("retry not verified: %s", result.Output)
	}
	if !strings.Contains(result.Output, "reclassified") {
		t.Errorf("retry output = %q, want it to reflect the real command that actually ran", result.Output)
	}
}

// The live bug this pins: a retry step written as a bare compound shell
// command ("python3 -m venv ... && pip install -r requirements.txt", no
// "run the command:" framing) reclassified to "general" (Classify()
// found no positive match), and the earlier reclassification fix trusted
// that outright - silently downgrading a real retry into a chat response
// that never actually runs anything, which the general skill still
// reports Verified:true. Absence of a positive match must never override
// the original action type; only an actual different classifier match
// does.
func TestSchedulerRetryKeepsOriginalTypeWhenNothingPositivelyReclassifies(t *testing.T) {
	registerTerminalSkill()

	// The ORIGINAL step is genuinely terminal work and fails for real
	// (a path that does not exist - not a missing program, which the
	// terminal hands to the operator instead of retrying). The mock's retry step has real shell
	// syntax but no "run the command:" lead phrase - Classify() alone
	// finds nothing for it and would resolve to "general" without the
	// fix, silently downgrading a real retry into an unverified chat
	// response.
	mock := &mockProvider{response: "DECISION: RETRY\nREASON: try a different real command\nRETRY_STEP: python3 -c \"print('reclass-test')\" && echo done"}
	sched := newTestScheduler(t, mock)

	job, err := sched.Run(context.Background(), "run this command: `ls /definitely/missing/path-xyz`", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := job.Results[0]
	if result.SkillName != "terminal" {
		t.Fatalf("retry ran through skill %q, want terminal - a bare command with no positive reclassification must keep the original action type, not silently become general chat", result.SkillName)
	}
	if !result.Verified {
		t.Fatalf("retry not verified: %s", result.Output)
	}
	if !strings.Contains(result.Output, "reclass-test") {
		t.Errorf("retry output = %q, want it to reflect the real command that actually ran", result.Output)
	}
}

// The real design gap this pins: a single-step RETRY structurally cannot
// fix some failures - "pip install X" failing on "environment is
// externally managed" needs a real venv CREATED as its own step and the
// install run as a SEPARATE step afterward; no amount of cleverness in
// one chained shell command fixes it, because "source venv/bin/activate"
// inside one exec.Command call has zero effect on a different command
// run afterward. DECOMPOSE asks the atomizer to break the fix into real
// steps and runs each one for real, rather than the model endlessly
// re-proposing smarter one-liners for a problem one line can't solve.
func TestSchedulerDecomposesRecoveryIntoRealSteps(t *testing.T) {
	registerTestFailFixtures()
	registerTerminalSkill()

	// One canned response serves both real model calls this path makes
	// (reasonAboutFailure, then atomizer.Atomize on the description) -
	// each parser only reads the lines with its own prefix.
	mock := &mockProvider{response: "DECISION: DECOMPOSE\n" +
		"REASON: needs a real environment step, then a real install step\n" +
		"RETRY_STEP: create a virtual environment, then install the package into it\n" +
		"TASK: yes\n" +
		"STEPS:\n" +
		"1. run the command: echo venv-step-ok\n" +
		"2. run the command: echo install-step-ok\n"}
	sched := newTestScheduler(t, mock)

	job, err := sched.Run(context.Background(), "trigger test-fail please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := job.Results[0]

	if result.Meta["decomposedRecovery"] != true {
		t.Fatalf("Meta[decomposedRecovery] not set, want true. Meta = %+v", result.Meta)
	}
	if !result.Verified {
		t.Fatalf("final recovery step not verified: %s", result.Output)
	}
	if !strings.Contains(result.Output, "install-step-ok") {
		t.Errorf("Output = %q, want it to reflect the real LAST decomposed step's output", result.Output)
	}
	// The concrete trail type is local to runDecomposedRecovery, so this
	// checks it structurally (via %+v) rather than asserting the type.
	trailStr := fmt.Sprintf("%+v", result.Meta["recoveryTrail"])
	if result.Meta["recoveryTrail"] == nil || !strings.Contains(trailStr, "venv-step-ok") || !strings.Contains(trailStr, "install-step-ok") {
		t.Errorf("Meta[recoveryTrail] = %v, want both real recovery steps recorded", trailStr)
	}
}

// DECOMPOSE must never recurse - a decomposed step that itself fails is
// recorded and the recovery moves on to the next step, never triggering
// its own round of failure-reasoning. Bounding this to one escalation is
// the whole point: real decomposition when needed, without opening the
// door to runaway step explosion.
func TestSchedulerDecomposedStepFailureDoesNotRecurse(t *testing.T) {
	registerTestFailFixtures()
	registerTerminalSkill()

	mock := &mockProvider{response: "DECISION: DECOMPOSE\n" +
		"REASON: needs two real steps\n" +
		"RETRY_STEP: do a thing that will fail, then a thing that will succeed\n" +
		"TASK: yes\n" +
		"STEPS:\n" +
		"1. run the command: this-command-does-not-exist-xyz\n" +
		"2. run the command: echo recovered-anyway\n"}
	sched := newTestScheduler(t, mock)

	callsBeforeRun := mock.calls
	job, err := sched.Run(context.Background(), "trigger test-fail please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := job.Results[0]

	// Exactly 2 real model calls total for this whole recovery: the
	// original reasonAboutFailure call, and the one atomizer.Atomize call
	// for the decomposition. A failing decomposed step must not trigger a
	// THIRD call by reasoning about ITS failure too.
	if got := mock.calls - callsBeforeRun; got != 2 {
		t.Errorf("made %d model call(s) for the whole recovery, want exactly 2 (no recursive reasoning about a decomposed step's own failure)", got)
	}
	if !result.Verified {
		t.Fatalf("final step not verified: %s", result.Output)
	}
	if !strings.Contains(result.Output, "recovered-anyway") {
		t.Errorf("Output = %q, want the last (successful) decomposed step's real output", result.Output)
	}
}

func realEmbedderForSkillTests(t *testing.T) *llm.OllamaProvider {
	t.Helper()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	p := llm.NewOllama(host)
	if !p.IsReachable(context.Background()) {
		t.Skipf("no real Ollama at %s - skill-matching tests need real embeddings", host)
	}
	return p
}

// The real point of the whole skill library: a confirmed skill's proven
// steps are reused DIRECTLY for a matching future request, instead of a
// fresh atomizer/LLM planning call every single time.
func TestSchedulerUsesAMatchedSkillInsteadOfFreshPlanning(t *testing.T) {
	registerTerminalSkill()
	embedder := realEmbedderForSkillTests(t)

	sched := newTestScheduler(t, &mockProvider{})
	lib, err := memory.NewSkillLibrary(sched.Workspace.Root(), embedder, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	proposed, err := lib.Propose(
		"a request to print a specific known-good greeting via the terminal",
		[]string{"run the command: `echo skill-library-worked`"}, "research")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if _, err := lib.Confirm(context.Background(), proposed.Slug); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	sched.SkillLib = lib

	job, err := sched.Run(context.Background(), "a request to print a specific known-good greeting via the terminal", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if job.UsedSkill != proposed.Slug {
		t.Errorf("job.UsedSkill = %q, want %q - the matched skill's steps should have driven the plan", job.UsedSkill, proposed.Slug)
	}
	if len(job.Results) != 1 || !job.Results[0].Verified {
		t.Fatalf("expected 1 verified result from the skill's real step, got %+v", job.Results)
	}
	if !strings.Contains(job.FinalAnswer, "skill-library-worked") {
		t.Errorf("FinalAnswer = %q, want the real output of the skill's own command", job.FinalAnswer)
	}
}

// A confirmed skill in the library must never affect a genuinely
// unrelated request - it should plan fresh, exactly as if no skill
// library were configured at all.
func TestSchedulerDoesNotUseAnUnrelatedSkill(t *testing.T) {
	registerTerminalSkill()
	embedder := realEmbedderForSkillTests(t)

	sched := newTestScheduler(t, &mockProvider{})
	lib, err := memory.NewSkillLibrary(sched.Workspace.Root(), embedder, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	proposed, err := lib.Propose("installing python packages when pip is externally managed",
		[]string{"run the command: `echo wrong-skill-used`"}, "research")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if _, err := lib.Confirm(context.Background(), proposed.Slug); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	sched.SkillLib = lib

	job, err := sched.Run(context.Background(), "run this command: `echo genuinely-unrelated-request`", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if job.UsedSkill != "" {
		t.Errorf("job.UsedSkill = %q, want empty - an unrelated request must not reuse a mismatched skill", job.UsedSkill)
	}
	if strings.Contains(job.FinalAnswer, "wrong-skill-used") {
		t.Error("the unrelated skill's command leaked into an unrelated request's plan")
	}
}

// The live bug this pins, found the first time the coding harness ran
// against a real VM: "Build a Python library that implements an LRU cache
// with a TTL..." matched a confirmed skill whose trigger was "Can you
// create a flask server file in python..." - both are "python, make me
// something", so their embeddings sit close enough to clear the
// similarity threshold - and the ENTIRE build was replaced by that
// skill's one recorded step, `python3 -m venv ...`. A real software
// project became a single venv command, and the job reported "done".
//
// Whole-request recipe reuse is simply the wrong granularity for building
// software: no two projects are the same project. What genuinely does
// transfer between builds is per-FAILURE knowledge, which the harness
// consults separately (internal/coding's Diagnose reads L1 with the real
// error text, mid-build).
func TestSchedulerNeverLetsAnAcquiredSkillHijackACodingRequest(t *testing.T) {
	registerTerminalSkill()
	embedder := realEmbedderForSkillTests(t)

	sched := newTestScheduler(t, &mockProvider{})
	lib, err := memory.NewSkillLibrary(sched.Workspace.Root(), embedder, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	// The real trigger text of the skill that actually caused this.
	proposed, err := lib.Propose(
		"Can you create a flask server file in python. Additionally run it. Run the server on port 5050.",
		[]string{"run the command: `echo skill-hijacked-the-build`"}, "decomposed-recovery")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if _, err := lib.Confirm(context.Background(), proposed.Slug); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	sched.SkillLib = lib

	job, err := sched.Run(context.Background(),
		"Build a Python library that implements an LRU cache with a TTL per entry, thread-safe, with get/put/delete/stats operations",
		nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if job.UsedSkill != "" {
		t.Errorf("job.UsedSkill = %q, want empty - a coding request must never be replaced by a whole-request recipe", job.UsedSkill)
	}
	if strings.Contains(job.FinalAnswer, "skill-hijacked-the-build") {
		t.Error("the acquired skill's command ran instead of the build - the exact live bug this test exists for")
	}
	if len(job.Plan) != 1 || job.Plan[0].Type != "coding" {
		t.Errorf("Plan = %+v, want exactly one coding step (the whole request, handed to the build harness)", job.Plan)
	}
}

// The real point of DECOMPOSE + the skill library together: a genuine
// fix, proven just now, gets proposed for future reuse - but only ever
// as a real, pending proposal a human still has to confirm.
func TestSchedulerProposesASkillAfterASuccessfulDecomposedRecovery(t *testing.T) {
	registerTestFailFixtures()
	registerTerminalSkill()
	embedder := realEmbedderForSkillTests(t)

	sched := newTestScheduler(t, &mockProvider{
		response: "DECISION: DECOMPOSE\n" +
			"REASON: needs a real fix\n" +
			"RETRY_STEP: do the real fix\n" +
			"TASK: yes\n" +
			"STEPS:\n" +
			"1. run the command: echo decomposed-fix-worked\n",
	})
	lib, err := memory.NewSkillLibrary(sched.Workspace.Root(), embedder, "test-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	sched.SkillLib = lib

	job, err := sched.Run(context.Background(), "trigger test-fail please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if job.ProposedSkill == "" {
		t.Fatal("job.ProposedSkill is empty, want a real proposal after a successful decomposed recovery")
	}

	pending, err := lib.ListPending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending() = %+v, err=%v, want exactly 1 real pending proposal on disk", pending, err)
	}
	if pending[0].Slug != job.ProposedSkill {
		t.Errorf("pending skill slug = %q, want it to match job.ProposedSkill = %q", pending[0].Slug, job.ProposedSkill)
	}
	// The proposal must not be usable yet - nothing is trusted before a
	// human confirms it.
	confirmed, _ := lib.ListConfirmed()
	if len(confirmed) != 0 {
		t.Errorf("ListConfirmed() = %+v, want empty - a proposal must never be auto-trusted", confirmed)
	}
}

func TestSchedulerAsksUserWhenLLMSaysAskUser(t *testing.T) {
	registerTestFailFixtures()

	mock := &mockProvider{response: "DECISION: ASK_USER\nREASON: genuinely need more information\nRETRY_STEP:"}
	sched := newTestScheduler(t, mock)

	job, err := sched.Run(context.Background(), "trigger test-fail please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	result := job.Results[0]
	if result.Meta["needsUserInput"] != true {
		t.Errorf("expected needsUserInput=true in Meta, got %+v", result.Meta)
	}
	if result.Verified {
		t.Error("Verified = true, want false (task was never actually completed)")
	}
}

func TestSchedulerHandlesLLMErrorGracefully(t *testing.T) {
	registerTestFailFixtures()

	mock := &mockProvider{err: errFakeUnreachable{}}
	sched := newTestScheduler(t, mock)

	job, err := sched.Run(context.Background(), "trigger test-fail please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() returned a hard error instead of an honest failed result: %v", err)
	}
	result := job.Results[0]
	if result.Meta["needsUserInput"] != true {
		t.Errorf("an LLM error during reasoning should fail safe to needsUserInput=true, got %+v", result.Meta)
	}
}

type errFakeUnreachable struct{}

func (errFakeUnreachable) Error() string { return "simulated: LLM unreachable" }

// fatalSkill reports a failure it knows retrying cannot fix - the shape a
// skill uses when the tool it depends on is simply unusable.
type fatalSkill struct{ runs int }

func (f *fatalSkill) Name() string                  { return "fatal-test" }
func (f *fatalSkill) Description() string           { return "always fails fatally" }
func (f *fatalSkill) Matches(s skills.Subtask) bool { return s.Type == "fatal-test" }
func (f *fatalSkill) Run(rc skills.RunContext, s skills.Subtask) (skills.Result, error) {
	f.runs++
	return skills.Result{
		Subtask: s, SkillName: f.Name(),
		Output:   "the model could not be used at all",
		Verified: false,
		Meta:     map[string]any{"fatal": true, "needsUserInput": true},
	}, nil
}

// Found live routing builds to a model the server could not load at all:
// every call died with "llama-server process has terminated ... CUDA
// error". The harness honestly refused to claim success - and then the
// scheduler reasoned about it and decomposed it, spending two more calls
// against the SAME dead model to reach the identical conclusion minutes
// later. Some failures genuinely cannot be retried into working.
func TestSchedulerDoesNotRetryAFailureMarkedFatal(t *testing.T) {
	fatal := &fatalSkill{}
	// Registered through the real classifier chain, like every other
	// fixture here, so Atomize resolves it deterministically and makes no
	// model call of its own - otherwise this test's "zero model calls"
	// assertion would really be measuring the atomizer.
	atomizer.Register("fatal-test", func(step string) bool { return step == "fatal-fixture please" })
	skills.Register(fatal)
	registerTerminalSkill()

	mock := &mockProvider{response: "DECISION: RETRY\nREASON: should never be asked\nRETRY_STEP: run the command: `echo should-not-run`"}
	sched := newTestScheduler(t, mock)

	callsBefore := mock.calls
	job, err := sched.Run(context.Background(), "fatal-fixture please", nil, func(string) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if fatal.runs != 1 {
		t.Errorf("the failing skill ran %d time(s), want exactly 1 - a fatal failure must not be retried", fatal.runs)
	}
	if got := mock.calls - callsBefore; got != 0 {
		t.Errorf("made %d model call(s) recovering from a fatal failure, want 0", got)
	}
	if strings.Contains(job.FinalAnswer, "should-not-run") {
		t.Error("the retry step ran anyway")
	}
	if job.Results[0].Meta["needsUserInput"] != true {
		t.Error("Meta[needsUserInput] not preserved - the user has to be told this needs them")
	}
}
