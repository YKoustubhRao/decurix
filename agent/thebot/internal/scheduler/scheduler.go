// Package scheduler runs an atomized plan sequentially, one subtask at a
// time, streaming real progress as it happens. On a subtask failure it
// makes one real, grounded LLM call to reason about why the failure
// happened and whether a different concrete approach could work -
// retrying with that approach if so, or surfacing that it genuinely needs
// the user's help if not. This is a first, real, working version of that
// loop, not its final word: it caps at one retry attempt for now rather
// than an open-ended reasoning loop.
package scheduler

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"thebot/internal/atomizer"
	"thebot/internal/feedback"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/rag"
	"thebot/internal/skills"
	"thebot/internal/workspace"
)

type Job struct {
	Prompt      string
	Plan        []skills.Subtask
	Results     []skills.Result
	FinalAnswer string
	Status      string // "done" | "error"
	// UsedSkill is set when a matched acquired skill's own recipe drove
	// this job's plan directly, instead of a fresh atomizer/LLM call -
	// real transparency for "did it actually reuse what it learned".
	UsedSkill string
	// ProposedSkill is set when this job's recovery succeeded in a way
	// worth remembering - a real, pending proposal now sitting on disk,
	// awaiting a human's confirm/reject before it is ever trusted again.
	ProposedSkill string
}

type Scheduler struct {
	Provider  llm.Provider
	Models    llm.Models
	Workspace *workspace.Workspace
	L2        *memory.L2
	// RAG, Feedback and L1 are all optional (nil-safe) - a Scheduler built
	// without them (e.g. in unit tests) still runs, it just skips
	// auto-ingestion, feedback consultation and memory recall rather than
	// panicking.
	RAG      *rag.RAG
	Feedback *feedback.Store
	L1       *memory.L1
	// SkillLib is acquired procedural memory (see internal/memory.
	// SkillLibrary) - optional and nil-safe like the other tiers above.
	// Set directly after New() rather than threaded through the
	// constructor; it was added later and every existing call site
	// already has seven positional args.
	SkillLib *memory.SkillLibrary
}

func New(provider llm.Provider, models llm.Models, ws *workspace.Workspace, l2 *memory.L2, dataRAG *rag.RAG, fb *feedback.Store, l1 *memory.L1) *Scheduler {
	return &Scheduler{Provider: provider, Models: models, Workspace: ws, L2: l2, RAG: dataRAG, Feedback: fb, L1: l1}
}

// Run executes one job end to end: atomize, run each subtask sequentially
// (with real failure-reasoning-and-retry), compose a final answer.
// onLog streams progress as it happens - never buffered until the end.
//
// sessionHistory is this UI conversation's real prior turns (may be nil
// for a request with no session), threaded down into every subtask's
// RunContext so a conversational skill actually has the conversation.
func (s *Scheduler) Run(ctx context.Context, prompt string, sessionHistory []llm.Message, onLog func(string)) (*Job, error) {
	job := &Job{Prompt: prompt}

	// Acquired skills before fresh planning: a confirmed skill is a
	// PROVEN recipe for a request shaped like this one, so reusing its
	// real steps directly beats re-deriving a plan (and re-researching
	// whatever it took to get that plan right the first time) from
	// scratch every single time the same class of problem comes up.
	//
	// A CODING request is the one exception, and it is a hard one. Found
	// live the first time the coding harness ran against a real VM:
	// "Build a Python library that implements an LRU cache with a TTL..."
	// matched an acquired skill whose trigger was "Can you create a flask
	// server file in python..." - both are "python, make me something", so
	// their embeddings sit close enough to clear the similarity threshold -
	// and the whole build was replaced by that skill's single recorded
	// step, `python3 -m venv /home/headless/ai/venv`. A real software
	// project became one venv command, and the job reported "done".
	//
	// Reusing a whole-request recipe is simply the wrong granularity for
	// building software: no two projects are the same project, and the
	// steps that got one of them working are not the steps for the next.
	// The learning that DOES transfer between builds - "this failure means
	// PEP 668, create a venv and install into it by path" - is per-FAILURE,
	// not per-request, and the harness already consults exactly that
	// (internal/coding's Diagnose reads L1 mid-build with the real error
	// text). So coding skips this layer entirely and keeps the one that
	// works at the right granularity.
	skipSkills := atomizer.Classify(prompt) == "coding"
	if skipSkills {
		onLog("skill library: skipping whole-request skill reuse for a coding task - the build harness consults memory per failure instead, which is the granularity that actually transfers")
	}

	if matched := s.matchSkillUnless(ctx, skipSkills, prompt, onLog); matched != nil {
		job.UsedSkill = matched.Slug
		for _, step := range matched.Steps {
			job.Plan = append(job.Plan, skills.Subtask{Step: step, Type: atomizer.Classify(step)})
		}
		onLog(fmt.Sprintf("Plan ready: %d step(s) to run (from the acquired skill).", len(job.Plan)))
	} else {
		plan := atomizer.Atomize(ctx, prompt, s.Provider, s.Models.Judgment, onLog)
		for _, step := range plan.Steps {
			job.Plan = append(job.Plan, skills.Subtask{Step: step, Type: atomizer.Classify(step)})
		}
		if plan.IsTask {
			onLog(fmt.Sprintf("Plan ready: %d step(s) to run.", len(job.Plan)))
		} else {
			onLog("Answering directly - no task steps needed.")
		}
	}

	// Recall before acting: whatever L1 already knows that's relevant to
	// this request, fetched once per job and threaded into every subtask.
	recalled := s.recallL1(ctx, prompt, onLog)

	// Real self-knowledge, fetched once per job the same way. Live bug:
	// asked "how much memory is your RAG using, how many documents and
	// chunks do you have indexed", the answer was "I don't have the
	// ability to check memory usage or document/chunk counts" - which was
	// simply false. The UI's own memory drawer gets this exact data from
	// RAG.Stats()/L1.Count()/L2.Count()/Feedback.Count() every time it
	// opens; the model itself just never had it in front of it.
	stats := s.selfStats(ctx)

	for _, subtask := range job.Plan {
		result := s.runSubtaskWithRecovery(ctx, subtask, job.Prompt, job.Results, sessionHistory, recalled, stats, onLog)
		job.Results = append(job.Results, result)
		if slug, ok := result.Meta["proposedSkill"].(string); ok && slug != "" {
			job.ProposedSkill = slug
		}
	}

	job.FinalAnswer = s.composeFinalAnswer(job)
	job.Status = "done"

	if s.L2 != nil {
		_ = s.L2.Append(memory.L2Entry{Prompt: job.Prompt, Status: job.Status, Plan: stepStrings(job.Plan)})
	}

	return job, nil
}

func stepStrings(plan []skills.Subtask) []string {
	out := make([]string, len(plan))
	for i, s := range plan {
		out[i] = s.Step
	}
	return out
}

func (s *Scheduler) runOnce(skill skills.Skill, rc skills.RunContext, subtask skills.Subtask, onLog func(string)) skills.Result {
	if skill == nil {
		onLog(fmt.Sprintf("no skill matched task type %q", subtask.Type))
		return skills.Result{
			Subtask: subtask, SkillName: "none",
			Output:   fmt.Sprintf("No skill available yet for this kind of task (%q).", subtask.Type),
			Verified: false, Meta: map[string]any{},
		}
	}
	onLog(fmt.Sprintf("scheduler: %q [%s] -> skill=%s model=%s", truncate(subtask.Step, 70), subtask.Type, skill.Name(), rc.Model))
	result, err := skill.Run(rc, subtask)
	if err != nil {
		onLog(fmt.Sprintf("skill %s returned an error: %s", skill.Name(), err.Error()))
		return skills.Result{Subtask: subtask, SkillName: skill.Name(), Output: err.Error(), Verified: false, Meta: map[string]any{"error": err.Error()}}
	}
	if result.Meta == nil {
		result.Meta = map[string]any{}
	}
	return result
}

func (s *Scheduler) runSubtaskWithRecovery(ctx context.Context, subtask skills.Subtask, originalPrompt string, priorResults []skills.Result, sessionHistory []llm.Message, recalledFacts []string, selfStats string, onLog func(string)) skills.Result {
	skill := skills.Pick(subtask)
	model := s.Models.ForTaskType(subtask.Type)
	rc := skills.RunContext{
		Ctx: ctx, OnLog: onLog, OriginalPrompt: originalPrompt,
		PriorResults: priorResults, Provider: s.Provider, Model: model, Workspace: s.Workspace,
		RAG: s.RAG, Feedback: s.Feedback, SessionHistory: sessionHistory, RecalledFacts: recalledFacts,
		SelfStats: selfStats,
		// Live memory handles, as opposed to the recalled snapshot above:
		// the coding harness queries L1 mid-build with a real error message
		// (which nothing could have recalled up front - the failure hadn't
		// happened yet) and writes back what it genuinely had to research.
		// Nil-safe on both sides.
		L1: s.L1, SkillLib: s.SkillLib,
	}

	before := s.snapshotWorkspace(onLog)
	result := s.runOnce(skill, rc, subtask, onLog)
	s.ingestChangedFiles(ctx, before, onLog)

	if result.Verified {
		return result
	}

	skillName := "none"
	if skill != nil {
		skillName = skill.Name()
	}
	onLog(fmt.Sprintf("subtask failed (%s) - reasoning about why before giving up...", skillName))

	pastFeedback := s.consultFeedback(ctx, subtask.Step, onLog)
	reason, retryStep, decision := s.reasonAboutFailure(ctx, subtask, result, pastFeedback)
	onLog(fmt.Sprintf("reasoning: %s", reason))

	if decision == recoverAskUser || retryStep == "" || retryStep == subtask.Step {
		result.Meta["needsUserInput"] = true
		result.Meta["failureReasoning"] = reason
		s.recordFailureFeedback(ctx, subtask.Step, result.Output+" | reasoning: "+reason, onLog)
		return result
	}

	if decision == recoverDecompose {
		return s.runDecomposedRecovery(ctx, subtask, result, reason, retryStep, rc, onLog)
	}

	onLog(fmt.Sprintf("retrying with a different approach: %s", retryStep))
	// Re-classify the retry step from what it actually says to do - never
	// inherit the failed attempt's type. Live bug: a rag-query step found
	// nothing indexed, the failure-reasoning model correctly suggested
	// retrying as "search the web for X", and the retry still ran through
	// the rag-query skill (same Type as the original), so it failed the
	// exact same way again instead of actually reaching the web.
	retryType := atomizer.Classify(retryStep)
	// Only trust a POSITIVE reclassification. Live bug this half fixes: a
	// retry step is meant to be "a different concrete approach that could
	// work" - always still an action - but the failure-reasoning model
	// often drops the leading verb phrasing a classifier needs ("run the
	// command: X" becomes just "X"). Classify() then finds nothing and
	// returns "general", which used to be trusted outright - silently
	// downgrading a real retry into a chat response that never actually
	// runs anything, and which the general skill still reports as
	// Verified:true. Absence of a positive match is not evidence the
	// retry became conversational; it's the classifier missing a cue it
	// had before. Only an ACTUAL different classifier match overrides the
	// original type - "general" alone never does.
	if retryType == "general" && subtask.Type != "general" {
		retryType = subtask.Type
	}
	if retryType != subtask.Type {
		onLog(fmt.Sprintf("scheduler: retry is a different kind of step (%s -> %s)", subtask.Type, retryType))
	}
	retrySubtask := skills.Subtask{Step: retryStep, Type: retryType}
	before2 := s.snapshotWorkspace(onLog)
	retryResult := s.runOnce(skills.Pick(retrySubtask), rc, retrySubtask, onLog)
	s.ingestChangedFiles(ctx, before2, onLog)
	retryResult.Meta["retriedAfterFailure"] = true
	retryResult.Meta["originalFailure"] = result.Output
	if !retryResult.Verified {
		s.recordFailureFeedback(ctx, subtask.Step, "original: "+result.Output+" | retry ("+retryStep+"): "+retryResult.Output, onLog)
	}
	return retryResult
}

// runDecomposedRecovery runs a real multi-step recovery: the atomizer
// breaks the failure-reasoning model's plain-language description of what
// needs to happen into concrete steps, and each one runs for real, in
// order, exactly once. No step here gets its own failure-reasoning
// retry loop - a decomposed step that itself fails is recorded and the
// recovery moves on, best-effort, rather than recursing into another
// round of reasoning-about-a-failure-about-a-failure. That's the whole
// point of bounding this to one escalation: real decomposition when it's
// genuinely needed, without opening the door to runaway step explosion.
func (s *Scheduler) runDecomposedRecovery(ctx context.Context, subtask skills.Subtask, original skills.Result, reason, description string, rc skills.RunContext, onLog func(string)) skills.Result {
	onLog(fmt.Sprintf("scheduler: decomposing the fix into real steps: %s", description))
	plan := atomizer.Atomize(ctx, description, s.Provider, s.Models.Judgment, onLog)

	if len(plan.Steps) == 0 {
		original.Meta["needsUserInput"] = true
		original.Meta["failureReasoning"] = reason
		s.recordFailureFeedback(ctx, subtask.Step, original.Output+" | reasoning: "+reason+" | decomposition produced no usable steps", onLog)
		return original
	}

	onLog(fmt.Sprintf("scheduler: recovery plan has %d step(s)", len(plan.Steps)))
	type recoveryStepSummary struct {
		Step     string `json:"step"`
		Skill    string `json:"skill"`
		Verified bool   `json:"verified"`
	}
	var trail []recoveryStepSummary
	last := original
	for i, step := range plan.Steps {
		recoveryType := atomizer.Classify(step)
		recoverySubtask := skills.Subtask{Step: step, Type: recoveryType}
		onLog(fmt.Sprintf("scheduler: recovery step %d/%d [%s]: %s", i+1, len(plan.Steps), recoveryType, step))

		beforeStep := s.snapshotWorkspace(onLog)
		stepResult := s.runOnce(skills.Pick(recoverySubtask), rc, recoverySubtask, onLog)
		s.ingestChangedFiles(ctx, beforeStep, onLog)

		trail = append(trail, recoveryStepSummary{Step: step, Skill: stepResult.SkillName, Verified: stepResult.Verified})
		last = stepResult
		if !stepResult.Verified {
			onLog(fmt.Sprintf("scheduler: recovery step %d/%d failed - continuing with the rest of the recovery plan", i+1, len(plan.Steps)))
		}
	}

	last.Meta["retriedAfterFailure"] = true
	last.Meta["decomposedRecovery"] = true
	last.Meta["originalFailure"] = original.Output
	last.Meta["recoveryTrail"] = trail
	if !last.Verified {
		s.recordFailureFeedback(ctx, subtask.Step, "original: "+original.Output+" | decomposed recovery's last step still failed: "+last.Output, onLog)
		return last
	}

	// A real fix, found and proven just now - worth remembering so the
	// same class of failure doesn't need the same research and
	// decomposition all over again next time. Proposed only, never
	// trusted automatically: this goes to .skills/pending/ and is matched
	// against nothing until a human confirms it. A fix that happens to
	// make an error go away is not automatically a fix that's SAFE to
	// silently reapply forever with no one ever looking at it again.
	if s.SkillLib != nil {
		trigger := fmt.Sprintf("%s (failed because: %s)", subtask.Step, reason)
		proposed, err := s.SkillLib.Propose(trigger, plan.Steps, "decomposed-recovery")
		if err != nil {
			onLog("skill library: could not propose a skill from this recovery - " + err.Error())
		} else {
			onLog(fmt.Sprintf("skill library: proposed a new skill %q from this recovery - awaiting your confirmation", proposed.Slug))
			last.Meta["proposedSkill"] = proposed.Slug
		}
	}
	return last
}

// snapshotWorkspace/ingestChangedFiles together are the real, literal
// implementation of "P0: whenever the AI creates or modifies a file, it
// calls P0 ingestion immediately for that file" - a before/after diff of
// every real file under the workspace around each subtask, with anything
// new or changed ingested into the Data RAG right away, synchronously, so
// it's visible in the same job's own log rather than a background queue
// (the standalone priority-queue daemon from the design is a later phase;
// this is the real, working version of "immediate" for now).
func (s *Scheduler) snapshotWorkspace(onLog func(string)) map[string]workspace.FileState {
	if s.RAG == nil {
		return nil
	}
	snap, err := s.Workspace.Snapshot()
	if err != nil {
		onLog(fmt.Sprintf("rag: could not snapshot workspace before running: %s", err.Error()))
		return nil
	}
	return snap
}

func (s *Scheduler) ingestChangedFiles(ctx context.Context, before map[string]workspace.FileState, onLog func(string)) {
	if s.RAG == nil || before == nil {
		return
	}
	after, err := s.Workspace.Snapshot()
	if err != nil {
		onLog(fmt.Sprintf("rag: could not snapshot workspace after running: %s", err.Error()))
		return
	}
	changed := workspace.DiffNewOrChanged(before, after)
	total := len(changed)
	for i, relPath := range changed {
		// "rag-progress:" prefix is a real, parseable contract the UI
		// renders as a live progress bar (see web/index.html) - i/total
		// are the real position in the real diff list, not estimated.
		onLog(fmt.Sprintf("rag-progress: ingesting (%d/%d): %s", i+1, total, relPath))
		chunks, method, err := s.RAG.IngestFile(ctx, relPath)
		if err != nil {
			onLog(fmt.Sprintf("rag: P0 ingest failed for %s: %s", relPath, err.Error()))
			continue
		}
		onLog(fmt.Sprintf("rag-progress: ingested (%d/%d): %s - %d chunk(s) via %s", i+1, total, relPath, chunks, method))
	}
}

// matchSkill checks acquired procedural memory before any fresh planning
// happens. Logged explicitly either way, matching how L1 recall is
// reported - "did it check" and "what came back" should never be a
// silent implementation detail.
// matchSkillUnless is matchSkill with a real, explicit opt-out, so the
// caller's reason for skipping lives next to the decision rather than
// being buried in a nil check here.
func (s *Scheduler) matchSkillUnless(ctx context.Context, skip bool, prompt string, onLog func(string)) *memory.AcquiredSkill {
	if skip {
		return nil
	}
	return s.matchSkill(ctx, prompt, onLog)
}

func (s *Scheduler) matchSkill(ctx context.Context, prompt string, onLog func(string)) *memory.AcquiredSkill {
	if s.SkillLib == nil {
		return nil
	}
	onLog("skill library: checking for an acquired skill that fits this request...")
	matches, err := s.SkillLib.Match(ctx, prompt, 1)
	if err != nil {
		onLog("skill library: match failed - " + err.Error())
		return nil
	}
	if len(matches) == 0 {
		onLog("skill library: no matching acquired skill")
		return nil
	}
	onLog(fmt.Sprintf("skill library: reusing acquired skill %q (similarity %.2f): %s", matches[0].Slug, matches[0].Score, matches[0].Trigger))
	return &matches[0]
}

// recallL1 fetches the curated facts relevant to this request. Logged
// explicitly (not silently) so the UI can show that memory was actually
// consulted and what came back.
func (s *Scheduler) recallL1(ctx context.Context, prompt string, onLog func(string)) []string {
	if s.L1 == nil {
		return nil
	}
	onLog("memory: checking L1 for anything relevant...")
	entries, err := s.L1.Recall(ctx, prompt, 5)
	if err != nil {
		onLog("memory: L1 recall failed: " + err.Error())
		return nil
	}
	if len(entries) == 0 {
		onLog("memory: nothing in L1 yet")
		return nil
	}
	facts := make([]string, 0, len(entries))
	for _, e := range entries {
		facts = append(facts, e.Fact)
	}
	onLog(fmt.Sprintf("memory: recalled %d fact(s) from L1", len(facts)))
	return facts
}

// selfStats is real introspection: exactly the same counts the UI's
// memory drawer shows, given to the model as fact instead of leaving it to
// guess (wrongly) that it has no way to know. Every source here is
// nil-safe and cheap (a handful of real COUNT-shaped queries), so this
// runs unconditionally rather than only when a question looks
// self-referential - regexing for "how much memory do you use" reliably
// is a losing game; just always having the real numbers on hand isn't.
func (s *Scheduler) selfStats(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("Your real current state, queried just now - use these exact numbers if asked about yourself:\n")

	if s.RAG != nil {
		if chunks, files, err := s.RAG.Stats(ctx); err == nil {
			fmt.Fprintf(&b, "- Data RAG: %d file(s) indexed, %d chunk(s) total.\n", files, chunks)
		}
	} else {
		b.WriteString("- Data RAG: not configured for this session.\n")
	}
	if s.L1 != nil {
		if n, err := s.L1.Count(ctx); err == nil {
			fmt.Fprintf(&b, "- Long-term memory (L1): %d fact(s) stored.\n", n)
		}
	}
	if s.L2 != nil {
		if n, err := s.L2.Count(); err == nil {
			fmt.Fprintf(&b, "- Job log (L2): %d job(s) recorded.\n", n)
		}
	}
	if s.Feedback != nil {
		if n, err := s.Feedback.Count(ctx); err == nil {
			fmt.Fprintf(&b, "- Negative feedback: %d entries recorded.\n", n)
		}
	}
	if s.Workspace != nil {
		fmt.Fprintf(&b, "- Workspace root: %s.\n", s.Workspace.Root())
	}
	return b.String()
}

func (s *Scheduler) consultFeedback(ctx context.Context, task string, onLog func(string)) []feedback.Entry {
	if s.Feedback == nil {
		return nil
	}
	entries, err := s.Feedback.ConsultNegative(ctx, task, 3)
	if err != nil {
		onLog(fmt.Sprintf("feedback: could not consult past negative feedback: %s", err.Error()))
		return nil
	}
	if len(entries) > 0 {
		onLog(fmt.Sprintf("feedback: found %d relevant past negative feedback entr(ies)", len(entries)))
	}
	return entries
}

// recordFailureFeedback writes one real, self-generated negative feedback
// entry - "source: self" distinguishes it from feedback a user gave
// explicitly (internal/skills/feedback.go), even though both land in the
// same store and are consulted identically.
func (s *Scheduler) recordFailureFeedback(ctx context.Context, task, detail string, onLog func(string)) {
	if s.Feedback == nil {
		return
	}
	if err := s.Feedback.RecordNegative(ctx, task, detail, "self"); err != nil {
		onLog(fmt.Sprintf("feedback: could not record negative feedback: %s", err.Error()))
		return
	}
	onLog("feedback: recorded a negative feedback entry for this failure")
}

var fieldRe = func(field string) *regexp.Regexp {
	return regexp.MustCompile(`(?im)^` + field + `:\s*(.+)$`)
}

var (
	decisionRe = fieldRe("DECISION")
	reasonRe   = fieldRe("REASON")
	retryRe    = fieldRe("RETRY_STEP")
)

// reasonAboutFailure makes one real, grounded LLM call: given the actual
// failed step, its actual output, and any relevant past negative
// feedback, decide whether a different concrete approach could work (and
// name it) or whether this genuinely needs the user. Never retries the
// identical thing blindly, and never ignores a known limitation it's been
// told about before.
// recoveryDecision is what reasonAboutFailure decided to do about a real
// failure - never more than one of these three, and DECOMPOSE only ever
// fires once per original subtask (runSubtaskWithRecovery does not call
// back into failure-reasoning for a step that came FROM a decomposition -
// see its comment). Unbounded "reason about the reasoning about the
// reasoning" is exactly the runaway-step-explosion risk that makes
// decomposition worth gating in the first place.
type recoveryDecision int

const (
	recoverAskUser recoveryDecision = iota
	recoverRetry
	recoverDecompose
)

// reasonAboutFailure makes one real, grounded judgment call about a real
// failure: is the fix a single concrete correction (RETRY), does it
// genuinely need to be broken into several real steps (DECOMPOSE), or is
// more information from the user actually required (ASK_USER).
//
// DECOMPOSE exists because a single-step retry structurally cannot fix
// some failures. Found live: "pip install X" failing with "environment is
// externally managed" got patched over and over with one increasingly
// elaborate compound shell command
// ("python3 -m venv v && source v/bin/activate && pip install X") stuffed
// into a single RETRY_STEP - which is really 2-3 distinct real actions,
// and "source" inside one exec.Command call has zero effect on any
// SEPARATE command run afterward, so the fix could never actually work no
// matter how it was worded. What's actually needed is real decomposition
// (create the venv, then install into it as its own step) - not a
// cleverer one-liner.
func (s *Scheduler) reasonAboutFailure(ctx context.Context, subtask skills.Subtask, result skills.Result, pastFeedback []feedback.Entry) (reason, retryStep string, decision recoveryDecision) {
	feedbackBlock := ""
	if len(pastFeedback) > 0 {
		var b strings.Builder
		b.WriteString("\n\nRelevant past negative feedback about you (take this seriously - do not repeat a known limitation):\n")
		for _, e := range pastFeedback {
			fmt.Fprintf(&b, "- (%s) task: %s | limitation: %s\n", e.Source, e.Task, e.Detail)
		}
		feedbackBlock = b.String()
	}

	prompt := fmt.Sprintf(`A task step failed for real. Reason about why, then decide ONE of:
1. RETRY - a single different concrete approach could fix this (give the exact new step to attempt)
2. DECOMPOSE - the fix genuinely needs MORE THAN ONE real step, not one cleverer command (e.g. a dependency install failing because of an isolated-environment restriction needs a real environment created as its own step, THEN the install as another - stuffing both into one chained command doesn't work, since each real step runs in its own fresh process and nothing like "activate" from an earlier command carries over). Describe what needs to happen in plain terms - do not try to write one command that does it all.
3. ASK_USER - you genuinely cannot proceed without more information from the user (say what you need)

Never propose a RETRY_STEP that abandons what was actually asked for something unrelated just because it would exit cleanly - a command that doesn't exist ("totally-not-a-real-command-xyz123") has no real fix to retry unless you're confident of the actual command that was meant; if you aren't, that is exactly what ASK_USER is for. A "successful" command that does something different from what was asked is not a real fix, it's a false success.

Failed step: %s
Real failure output:
%s%s

Respond in EXACTLY this format, nothing else:
DECISION: RETRY or DECOMPOSE or ASK_USER
REASON: <one or two sentences>
RETRY_STEP: <the new step (RETRY) or plain description of what needs to happen (DECOMPOSE); leave blank for ASK_USER>`, subtask.Step, result.Output, feedbackBlock)

	resp, err := s.Provider.Chat(ctx, s.Models.Judgment, []llm.Message{{Role: "user", Content: prompt}}, llm.Options{TimeoutMs: 60000})
	if err != nil {
		return fmt.Sprintf("could not reason about the failure (%s) - stopping here", err.Error()), "", recoverAskUser
	}

	rawDecision := strings.ToUpper(extractField(decisionRe, resp.Text))
	reason = extractField(reasonRe, resp.Text)
	retryStep = extractField(retryRe, resp.Text)

	if retryStep == "" {
		return reason, "", recoverAskUser
	}
	if strings.Contains(rawDecision, "DECOMPOSE") {
		return reason, retryStep, recoverDecompose
	}
	if strings.Contains(rawDecision, "RETRY") {
		return reason, retryStep, recoverRetry
	}
	return reason, "", recoverAskUser
}

func extractField(re *regexp.Regexp, text string) string {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// composeFinalAnswer: single-step jobs (everything routed by Atomize()
// today, since real multi-step decomposition is a later phase) just
// surface that one real result directly - no LLM paraphrasing needed
// when there's only one thing to report.
func (s *Scheduler) composeFinalAnswer(job *Job) string {
	if len(job.Results) == 1 {
		r := job.Results[0]
		if r.Error != "" {
			return fmt.Sprintf("[failed: %s]", r.Error)
		}
		return r.Output
	}
	var b strings.Builder
	for i, r := range job.Results {
		fmt.Fprintf(&b, "Subtask %d (%s):\n%s\n\n", i+1, r.Subtask.Step, r.Output)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
