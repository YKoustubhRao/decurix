package atomizer

import (
	"context"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// stubProvider stands in for a real model so planning behaviour can be
// tested deterministically, without a live Ollama or model variance.
type stubProvider struct {
	response string
	err      error
	calls    int
	gotOpts  llm.Options
}

func (s *stubProvider) Name() string { return "stub" }
func (s *stubProvider) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	s.calls++
	s.gotOpts = opts
	if s.err != nil {
		return llm.Result{}, s.err
	}
	return llm.Result{Text: s.response}, nil
}
func (s *stubProvider) IsReachable(ctx context.Context) bool { return s.err == nil }

func TestClassifyTerminalTasks(t *testing.T) {
	for _, step := range []string{
		"run this command: `echo hi`",
		"execute the script now",
		"please use the terminal",
		"run it again",
	} {
		if got := Classify(step); got != "terminal" {
			t.Errorf("Classify(%q) = %q, want terminal", step, got)
		}
	}
}

func TestClassifyWebTasks(t *testing.T) {
	for _, step := range []string{
		"search the web for RTX 5080 laptops",
		"look up the latest news on nvidia online",
		"https://example.com/page",
		"google for golang generics for me",
	} {
		if got := Classify(step); got != "web" {
			t.Errorf("Classify(%q) = %q, want web", step, got)
		}
	}
}

// file-write keeps everything that is real CONTENT rather than real CODE -
// notes, summaries, data. Code moved to the coding classifier below, which
// actually builds and tests it instead of saving one unexecuted completion.
func TestClassifyFileWriteTasks(t *testing.T) {
	for _, step := range []string{
		"save it to notes.txt",
		"write a short summary of the findings to summary.md",
		"write the results to results.csv",
	} {
		if got := Classify(step); got != "file-write" {
			t.Errorf("Classify(%q) = %q, want file-write", step, got)
		}
	}
}

// Anything that is really SOFTWARE goes to the coding harness, including
// the single-file cases file-write used to claim: a .py file full of code
// that nothing ever ran is exactly the failure the harness exists to fix.
func TestClassifyCodingTasks(t *testing.T) {
	for _, step := range []string{
		"write a segment tree implementation into segment_tree.py",
		"create a python script named fib.py",
		"build a REST API with FastAPI for managing tasks",
		"build a streamlit dashboard showing sales data",
		"implement quicksort in C++",
		"write a Go CLI tool that counts lines in a file",
		"create a docker-compose setup with postgres",
		"fix the bug in the parser",
		"refactor the payment module",
		"add unit tests for the cache layer",
		"write me some code to parse ISO timestamps",
	} {
		if got := Classify(step); got != "coding" {
			t.Errorf("Classify(%q) = %q, want coding", step, got)
		}
	}
}

// The coding classifier must never hijack a QUESTION about software. A
// full architect-scaffold-implement-test build is an absurd answer to
// "explain how a segment tree works".
func TestClassifyCodingDoesNotStealQuestions(t *testing.T) {
	for _, step := range []string{
		"explain how a segment tree works",
		"what is the difference between a list and a tuple in python",
		"how does the go scheduler work",
		"compare fastapi and flask",
		"tell me about rust's borrow checker",
	} {
		if got := Classify(step); got == "coding" {
			t.Errorf("Classify(%q) = coding, want anything but coding - this is a question, not a build request", step)
		}
	}
}

// Real false positives this suite caught the moment the coding classifier
// was added - both would have started a whole software build for work that
// is explicitly one command or one environment chore.
func TestClassifyCodingDoesNotStealCommandsOrEnvironmentWork(t *testing.T) {
	for _, step := range []string{
		"run the command: python3 setup.py build",
		"create a virtual environment, then install the package into it",
		"install the requests package",
		"pip install fastapi",
	} {
		if got := Classify(step); got == "coding" {
			t.Errorf("Classify(%q) = coding, want anything but coding - this is a command/environment step, not a build request", step)
		}
	}
}

func TestClassifyDownloadTasks(t *testing.T) {
	step := "download all the excel files from https://example.com/stats"
	if got := Classify(step); got != "download" {
		t.Errorf("Classify(%q) = %q, want download", step, got)
	}
}

func TestClassifyFallsThroughToGeneral(t *testing.T) {
	for _, step := range []string{
		"what is the capital of France?",
		"hello there",
		"what is 1+1?",
	} {
		if got := Classify(step); got != "general" {
			t.Errorf("Classify(%q) = %q, want general", step, got)
		}
	}
}

// The live bug: "write the fastest C++ implementation of X ... if you
// don't know one, search the web to find it" has two distinct verbs
// (write, search) joined only by a conditional-fallback clause, not an
// and/then joiner - looksCompound() missed it, so the single-atomic "web"
// bypass swallowed the whole request (including the file-write half) as
// one research-and-answer-in-text step, and genuinely good research still
// ended with the code dumped into chat instead of written to a file.
func TestLooksCompoundRecognisesConditionalFallbackPhrasing(t *testing.T) {
	if !looksCompound("write the fastest C++ implementation of X. If you don't know one, search the web to find it.") {
		t.Error("looksCompound() = false for a conditional-fallback compound request, want true")
	}
	if !looksCompound("write a solution to Y - if you aren't sure, research it online") {
		t.Error("looksCompound() = false for 'if you aren't sure', want true")
	}
}

// The live bug: "Build a C++ codebase using templates for a segment
// tree, FFT, and a Fenwick tree, each in their own file" never matched
// file-write at all (no literal filename anywhere, verb is "build" not
// "write/save/put") and fell through to plain chat, which printed the
// entire codebase back as a chat answer instead of ever touching disk -
// exactly the "must not print the code back" failure this whole system
// exists to prevent.
// It now routes to the coding harness rather than file-write, which is a
// strictly better answer to the same bug: the harness builds each
// component as its own real file AND compiles and tests every one of them,
// where file-write could only ever save whatever a model emitted in one
// shot, unexecuted.
func TestClassifyRoutesMultiFileBuildRequestsToTheCodingHarness(t *testing.T) {
	for _, step := range []string{
		"Build a C++ codebase using templates for a segment tree, FFT (fast fourier transform), and a Fenwick tree, each in their own file",
		"implement a trie and an AVL tree, each in its own file",
		"code a stack and a queue as separate files",
		"write a linked list and a hash map, one file each",
	} {
		if got := Classify(step); got != "coding" {
			t.Errorf("Classify(%q) = %q, want coding", step, got)
		}
	}
}

// A multi-file request must be treated as compound even though it's a
// single repeated verb ("build" applied to 3 components) - otherwise the
// single-atomic-step bypass hands the whole thing to file-write as ONE
// write call, silently producing only one of the requested files.
func TestLooksCompoundRecognisesExplicitMultiFileRequests(t *testing.T) {
	if !looksCompound("build a C++ codebase for a segment tree, FFT, and a Fenwick tree, each in their own file") {
		t.Error("looksCompound() = false for an explicit multi-file request, want true")
	}
	// A single ordinary file-write must NOT be forced compound just
	// because it mentions "file".
	if looksCompound("write a segment tree into segment_tree.py") {
		t.Error("looksCompound() = true for a single-file write, want false")
	}
}

// The live bug: "What's the latest AI news?" got "I don't have real-time
// capabilities... I recommend checking a news website" instead of the
// agent going and checking one itself, because the old pattern required
// "latest" and "news" adjacent - "latest AI news" has a word between them
// and never matched. A qualifying word (or several) between "latest" and
// the noun is completely ordinary phrasing and must still be caught.
func TestClassifyRoutesLatestXNewsToWeb(t *testing.T) {
	for _, step := range []string{
		"What's the latest AI news?",
		"what's the latest crypto news",
		"What's the latest story about Elon Musk? What's he up to these days?",
		"what is the latest Go version?",
		"latest smartphone prices",
	} {
		if got := Classify(step); got != "web" {
			t.Errorf("Classify(%q) = %q, want web", step, got)
		}
	}
}

// The live bug: right after bulk-ingesting real pages about a specific
// person, "who is X" in a brand new session went straight to a fresh web
// search instead of checking what was already indexed. "Who is"/"tell me
// about"/"what do you know about" phrasing must route to rag-query first.
func TestClassifyRoutesIdentityQuestionsToRAGQuery(t *testing.T) {
	for _, step := range []string{
		"Who is Abhishek Gadia?",
		"who is Abhishek Gadia? What companies is he associated with?",
		"tell me about Abhishek Gadia",
		"what do you know about Abhishek Gadia",
		"What can you tell me about Qube Research?",
	} {
		if got := Classify(step); got != "rag-query" {
			t.Errorf("Classify(%q) = %q, want rag-query", step, got)
		}
	}
}

// Ordinary questions must not get swept into rag-query just because they
// share a word with the new patterns.
func TestClassifyIdentityRuleDoesNotOvermatch(t *testing.T) {
	for _, step := range []string{
		"what is the capital of France?",
		"what is 1+1?",
		"what is my name?",
	} {
		if got := Classify(step); got == "rag-query" {
			t.Errorf("Classify(%q) = rag-query, want something else - this isn't a 'who is'/'tell me about' question", step)
		}
	}
}

// A prompt a classifier already claims must NOT cost a model call - that
// is the whole point of the deterministic bypass chain.
func TestAtomizeSkipsPlanningForClassifiedTasks(t *testing.T) {
	stub := &stubProvider{response: "TASK: yes\nSTEPS:\n1. something"}
	plan := Atomize(context.Background(), "run this command: `echo hi`", stub, "m", nil)

	if stub.calls != 0 {
		t.Errorf("made %d model call(s) for an already-classified task, want 0", stub.calls)
	}
	if !plan.IsTask || len(plan.Steps) != 1 {
		t.Errorf("plan = %+v, want a single-step task", plan)
	}
}

func TestAtomizePlansRealMultiStepTask(t *testing.T) {
	stub := &stubProvider{response: "TASK: yes\nSTEPS:\n1. search the web for X\n2. write the summary into notes.md"}
	plan := Atomize(context.Background(), "research X and write me a summary file", stub, "m", nil)

	if !plan.IsTask {
		t.Fatalf("IsTask = false, want true. plan = %+v", plan)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("got %d steps, want 2: %+v", len(plan.Steps), plan.Steps)
	}
	if plan.Steps[0] != "search the web for X" || plan.Steps[1] != "write the summary into notes.md" {
		t.Errorf("steps not parsed correctly: %+v", plan.Steps)
	}
}

func TestAtomizeTreatsPlainQuestionAsChat(t *testing.T) {
	stub := &stubProvider{response: "TASK: no"}
	plan := Atomize(context.Background(), "who wrote the odyssey?", stub, "m", nil)

	if plan.IsTask {
		t.Errorf("IsTask = true for a plain question, want false")
	}
	if len(plan.Steps) != 1 || plan.Steps[0] != "who wrote the odyssey?" {
		t.Errorf("plan.Steps = %+v, want the original prompt as one step", plan.Steps)
	}
}

// A planning failure must degrade to answering, never block the request.
func TestAtomizeFallsBackWhenPlanningFails(t *testing.T) {
	stub := &stubProvider{err: errStub{}}
	plan := Atomize(context.Background(), "some ambiguous thing", stub, "m", nil)

	if plan.IsTask {
		t.Errorf("IsTask = true after a failed planning call, want false")
	}
	if len(plan.Steps) != 1 {
		t.Errorf("Steps = %+v, want the original prompt preserved", plan.Steps)
	}
}

func TestAtomizeCapsStepCount(t *testing.T) {
	stub := &stubProvider{response: "TASK: yes\nSTEPS:\n1. a\n2. b\n3. c\n4. d\n5. e\n6. f\n7. g\n8. h\n9. i\n10. j\n11. k\n12. l"}
	var logs []string
	plan := Atomize(context.Background(), "do a big thing", stub, "m", func(l string) { logs = append(logs, l) })
	if len(plan.Steps) != maxSteps {
		t.Errorf("got %d steps, want capped at %d", len(plan.Steps), maxSteps)
	}
	// The cap must never be silent - a live bug had it quietly drop a real
	// write-to-file step with nothing anywhere saying so.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "capping at") {
			found = true
		}
	}
	if !found {
		t.Error("no log line announced the step cap - a real, valid step being dropped must never be silent")
	}
}

// The live bug this pins: "write C++ implementations of X, Y, and Z, each
// in its own file, searching the web first for any you don't know well"
// legitimately needs up to 2 steps per item - 3 items is 6 steps, and the
// old cap of 5 silently dropped the last write-to-file step. The dropped
// step's preceding search result still included example code in its own
// prose, so that code ended up in chat instead of ever reaching a file.
// Still a real risk, just on a non-coding request now that coding never
// reaches the step planner at all (see the handover test below).
func TestAtomizeCapAllowsEnoughStepsForSearchThenWritePerItem(t *testing.T) {
	stub := &stubProvider{response: "STEPS:\n" +
		"1. search the web for Edmonds' Blossom algorithm\n" +
		"2. write a summary of Edmonds' Blossom algorithm to blossom.md\n" +
		"3. search the web for Li Chao tree\n" +
		"4. write a summary of the Li Chao tree to li_chao_tree.md\n" +
		"5. search the web for centroid decomposition\n" +
		"6. write a summary of centroid decomposition to centroid_decomposition.md"}
	plan := Atomize(context.Background(),
		"research Blossom's algorithm, a Li Chao tree, and centroid decomposition, and write a summary of each in separate files, searching the web first for any you don't know well",
		stub, "m", nil)

	if len(plan.Steps) != 6 {
		t.Fatalf("got %d steps, want all 6 (search+write per item) preserved: %+v", len(plan.Steps), plan.Steps)
	}
	if !strings.Contains(plan.Steps[5], "centroid_decomposition.md") {
		t.Errorf("last step = %q, want the final write-to-file step preserved, not dropped by the cap", plan.Steps[5])
	}
}

// A coding request is handed to the harness WHOLE, however compound it
// looks. Splitting it first actively breaks things: each fragment would
// classify as "coding" on its own and spin up a separate architect-
// scaffold-test build in a separate directory, none of them aware of the
// others. The harness does its own, far better decomposition internally -
// it orders files by their real dependencies and tests each one.
func TestAtomizeHandsCodingRequestsToTheHarnessWhole(t *testing.T) {
	// A stub that would happily split it, to prove the handover happens
	// BEFORE any planning call rather than depending on what a model says.
	stub := &stubProvider{response: "STEPS:\n1. write models.py\n2. write main.py\n3. write tests\n"}
	prompt := "build a FastAPI service with a SQLite store, a REST layer and tests, each in its own file"

	plan := Atomize(context.Background(), prompt, stub, "m", nil)

	if len(plan.Steps) != 1 {
		t.Fatalf("got %d steps, want exactly 1 (the whole request): %+v", len(plan.Steps), plan.Steps)
	}
	if plan.Steps[0] != prompt {
		t.Errorf("step = %q, want the original request passed through untouched", plan.Steps[0])
	}
	if !plan.IsTask {
		t.Error("IsTask = false, want true - a build request is real work")
	}
}

func TestRegisterExtendsChain(t *testing.T) {
	before := len(registry)
	Register("test-only-type", func(step string) bool { return step == "__unit_test_marker__" })
	if len(registry) != before+1 {
		t.Fatalf("Register() did not extend the chain")
	}
	if got := Classify("__unit_test_marker__"); got != "test-only-type" {
		t.Errorf("Classify() = %q, want test-only-type", got)
	}
	registry = registry[:before]
}

type errStub struct{}

func (errStub) Error() string { return "simulated planning failure" }

// looksCompound is the guard for the real bug where "find out X and save
// it to Y" got claimed by the file-write classifier and ran as one step,
// so the "find out" half never happened.
func TestLooksCompoundDetectsMultiActionRequests(t *testing.T) {
	compound := []string{
		"Find out what the newest Go version is and save a short note about it to go-version.md",
		"search the web for RTX 5080 prices and write them into prices.md",
		"download the excel files and then summarize them",
	}
	for _, p := range compound {
		if !looksCompound(p) {
			t.Errorf("looksCompound(%q) = false, want true", p)
		}
	}

	single := []string{
		"write a segment tree into segment_tree.py",
		"search the web for RTX 5080 laptops",
		"run this command: `echo hi`",
		"what is my name?",
	}
	for _, p := range single {
		if looksCompound(p) {
			t.Errorf("looksCompound(%q) = true, want false", p)
		}
	}
}

// The live regression: the planner emitted human-desktop steps, none of
// which matched a skill, so they all fell through to plain chat and the
// agent answered "I can't access web pages". Anything naming a site is
// web work.
func TestClassifyRoutesSiteStepsToWeb(t *testing.T) {
	for _, step := range []string{
		"Go to the official Go website (https://golang.org/dl/).",
		"visit the downloads page on the site",
		"check https://example.com/prices",
		"open the web page and note the version",
	} {
		if got := Classify(step); got != "web" {
			t.Errorf("Classify(%q) = %q, want web - a step naming a site must reach the web skill", step, got)
		}
	}
}

// Steps naming a file still belong to file-write even when they mention a
// URL, so the site rule must not steal them.
func TestClassifySiteRuleDoesNotStealFileWrites(t *testing.T) {
	step := "write a note about https://golang.org/dl/ to go-version.md"
	if got := Classify(step); got != "file-write" {
		t.Errorf("Classify(%q) = %q, want file-write", step, got)
	}
}

// The live bug this pins: the planner emitted "repeat steps 4-6 until the
// command runs cleanly" as its own step. No skill can execute a loop
// instruction, so it fell through to plain chat, which - with nothing
// concrete to act on - apologized ("I don't have direct access...") and
// fabricated an entirely unrelated code sample. A loop-instruction step
// must be dropped like any other no-op, not dispatched at all.
func TestDropNoOpStepsCatchesRepeatInstructions(t *testing.T) {
	for _, step := range []string{
		"repeat steps 4-6 until the command runs cleanly",
		"repeat this until it works",
		"repeat steps 1-3",
	} {
		got := dropNoOpSteps([]string{step})
		if len(got) != 0 {
			t.Errorf("dropNoOpSteps([%q]) = %v, want it dropped as a loop instruction no skill can execute", step, got)
		}
	}
}

func TestDropNoOpSteps(t *testing.T) {
	in := []string{
		"search the web for the newest Go version",
		"Open a text editor and create a new file named go-version.md.",
		"write a short note to go-version.md",
		"Save the file.",
		"Verify the output.",
	}
	got := dropNoOpSteps(in)

	// "Open a text editor and create a new file named go-version.md" is
	// real work (it names a file), so only the pure no-ops go.
	want := []string{
		"search the web for the newest Go version",
		"Open a text editor and create a new file named go-version.md.",
		"write a short note to go-version.md",
	}
	if len(got) != len(want) {
		t.Fatalf("dropNoOpSteps() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A plan made entirely of no-ops must degrade to answering, never run as
// an empty task.
func TestAtomizeAllNoOpStepsFallsBackToAnswering(t *testing.T) {
	stub := &stubProvider{response: "TASK: yes\nSTEPS:\n1. Save the file.\n2. Done"}
	plan := Atomize(context.Background(), "tidy things up please", stub, "m", nil)
	if plan.IsTask {
		t.Errorf("IsTask = true for a plan of pure no-ops, want false")
	}
}

func TestAtomizePlansCompoundRequestInsteadOfBypassing(t *testing.T) {
	stub := &stubProvider{response: "TASK: yes\nSTEPS:\n1. search the web for the newest Go version\n2. write a note to go-version.md"}
	plan := Atomize(context.Background(),
		"Find out what the newest Go version is and save a short note about it to go-version.md",
		stub, "m", nil)

	if stub.calls != 1 {
		t.Errorf("made %d planning call(s) for a compound request, want 1 - it must not bypass to a single skill", stub.calls)
	}
	if len(plan.Steps) != 2 {
		t.Errorf("got %d steps, want 2: %+v", len(plan.Steps), plan.Steps)
	}
}

// The core live bug this pins: a classifier already matched a real action
// verb (this is unambiguously real work), but the model's step-planning
// response tried to answer as if re-litigating whether it's a task at all
// - a leftover "TASK: no"-shaped refusal habit. Once a classifier has
// matched, IsTask must never depend on the model agreeing again; only the
// step breakdown does.
func TestAtomizeNeverLetsModelOverrideAnAlreadyMatchedClassifier(t *testing.T) {
	// A deliberately unhelpful/refusal-shaped response, exactly the shape
	// that broke this live: no STEPS at all, just a "TASK: no" habit
	// bleeding into a prompt that never even offered that option.
	stub := &stubProvider{response: "TASK: no\nI cannot assist with downloading or scraping web content."}
	plan := Atomize(context.Background(),
		"search the web for Abhishek Gadia and download the raw html of the top 10 pages using a browser",
		stub, "m", nil)

	if !plan.IsTask {
		t.Errorf("IsTask = false, want true - a classifier (download/web) already matched, the model must not be able to override that")
	}
	// No usable steps were parseable from the refusal-shaped response, so
	// it must degrade to running the whole request as one atomic step -
	// never to "this was just a question".
	if len(plan.Steps) != 1 || plan.Steps[0] == "" {
		t.Errorf("plan.Steps = %+v, want the original request preserved as one atomic step", plan.Steps)
	}
}

// The step-planning call for an already-matched classifier must never ask
// "is this a task" - only how many steps. Regression for the exact prompt
// that produced the "TASK: no" refusal.
func TestAtomizeStepsOnlyPromptNeverAsksTaskVsQuestion(t *testing.T) {
	stub := &stubProvider{response: "STEPS:\n1. search the web for Abhishek Gadia\n2. download the raw html files from the search results"}
	plan := Atomize(context.Background(),
		"search the web for Abhishek Gadia and download the raw html of the top 10 pages using a browser",
		stub, "m", nil)

	if !plan.IsTask {
		t.Fatalf("IsTask = false, want true")
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("got %d steps, want 2: %+v", len(plan.Steps), plan.Steps)
	}
	if stub.calls != 1 {
		t.Errorf("made %d model call(s), want exactly 1 (steps-only, no separate task-vs-question call)", stub.calls)
	}
}

// The live bug this pins: the SAME planning prompt, sent twice at the
// provider's default temperature, came back "TASK: yes" once and
// "TASK: no" the next time - a structured yes/no classification call has
// no business running at creative-writing temperature.
func TestAtomizeUsesLowTemperatureForPlanning(t *testing.T) {
	stub := &stubProvider{response: "TASK: no"}
	Atomize(context.Background(), "some ambiguous request", stub, "m", nil)

	if stub.gotOpts.Temperature <= 0 || stub.gotOpts.Temperature > 0.3 {
		t.Errorf("planning call Temperature = %v, want a low deterministic value (0 < t <= 0.3)", stub.gotOpts.Temperature)
	}
}

// The log line shown in the thinking view must reflect reality: a
// classifier DID match a compound request (it just needs multi-step
// planning anyway), so the line must never claim "no classifier claimed
// this" in that case - that was simply false and misrepresented the
// agent's own reasoning to anyone watching the thinking view.
func TestAtomizeLogReflectsWhichClassifierMatchedForCompoundRequests(t *testing.T) {
	stub := &stubProvider{response: "TASK: yes\nSTEPS:\n1. search the web for the newest Go version\n2. look up the current Bitcoin price"}
	var logs []string
	Atomize(context.Background(), "search the web for the newest Go version and also look up the current Bitcoin price", stub, "m", func(l string) { logs = append(logs, l) })

	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, "no classifier claimed this") {
		t.Errorf("log claimed no classifier matched, but the web classifier did (it's just compound). logs:\n%s", joined)
	}
	if !strings.Contains(joined, `"web"`) {
		t.Errorf("log never named which classifier actually matched. logs:\n%s", joined)
	}
}
