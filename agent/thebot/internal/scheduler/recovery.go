package scheduler

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"thebot/internal/atomizer"
	"thebot/internal/feedback"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/skills"
)

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

// runStep runs one step with its recovery: a reroute to the operator when
// a conversational answer turns out to need the machine, then - for a real
// failure - one grounded reasoning call that retries, decomposes, or asks.
func (s *Scheduler) runStep(rc skills.RunContext, subtask skills.Subtask, planner atomizer.Planner) skills.Result {
	onLog := rc.OnLog
	rc.Model = s.Models.ForTaskType(subtask.Type)
	skill := skills.Pick(subtask)
	result := s.runOnce(skill, rc, subtask, onLog)
	// An approval is for one run of one action: whatever this run did not use is gone, so a retry, a reroute or a
	// decomposed recovery that reaches the same action is stopped and asked again.
	rc.Approved = nil

	if result.Verified {
		return result
	}

	// Chat discovered it has to look at the machine, or the terminal that
	// the program it was asked to run is not installed. Not a failure - the
	// wrong skill for the job, found out for the price of one short call or
	// one command. The operator gets the same words.
	if reroute, _ := result.Meta["reroute"].(string); reroute != "" && reroute != subtask.Type {
		onLog(fmt.Sprintf("scheduler: rerouting to the %s", reroute))
		step := subtask.Step
		// What the first skill found out that the next one must know
		// ("curl is not installed here").
		if note, _ := result.Meta["rerouteNote"].(string); note != "" {
			step += "\n\n" + note
		}
		return s.runStep(rc, skills.Subtask{Step: step, Type: reroute}, planner)
	}

	skillName := "none"
	if skill != nil {
		skillName = skill.Name()
	}

	// A pause is not a failure. The safety gate stopped an action, or the
	// skill itself asked the human something - reasoning about "why it
	// failed" would second-guess a decision that belongs to a person.
	if approval, _ := result.Meta["needsApproval"].(bool); approval {
		return result
	}
	if asked, _ := result.Meta["question"].(string); asked != "" {
		return result
	}

	// Some failures are genuinely not recoverable by trying again, and a
	// skill that knows that says so. Reasoning about an unusable model,
	// then decomposing the request, then running it through the SAME
	// unusable model is two extra model calls and several minutes spent
	// reaching the identical conclusion.
	if fatal, _ := result.Meta["fatal"].(bool); fatal {
		onLog(fmt.Sprintf("subtask failed (%s) and reported it as unrecoverable - not retrying", skillName))
		s.recordFailureFeedback(rc.Ctx, subtask.Step, result.Output, onLog)
		return result
	}

	// Out of time: there is no time left to reason about it either.
	if rc.Ctx.Err() != nil {
		return result
	}

	onLog(fmt.Sprintf("subtask failed (%s) - reasoning about why before giving up...", skillName))

	pastFeedback := s.consultFeedback(rc.Ctx, subtask.Step, onLog)
	reason, retryStep, decision := s.reasonAboutFailure(rc.Ctx, planner.System, subtask, result, pastFeedback)
	onLog(fmt.Sprintf("reasoning: %s", reason))

	if decision == recoverAskUser || retryStep == "" || retryStep == subtask.Step {
		result.Meta["needsUserInput"] = true
		result.Meta["failureReasoning"] = reason
		s.recordFailureFeedback(rc.Ctx, subtask.Step, result.Output+" | reasoning: "+reason, onLog)
		return result
	}

	if decision == recoverDecompose {
		return s.runDecomposedRecovery(rc, subtask, result, reason, retryStep, planner)
	}

	onLog(fmt.Sprintf("retrying with a different approach: %s", retryStep))
	// Re-classify the retry step from what it actually says to do - never
	// inherit the failed attempt's type. Live bug: a rag-query step found
	// nothing indexed, the failure-reasoning model correctly suggested
	// retrying as "search the web for X", and the retry still ran through
	// the rag-query skill, failing the exact same way again.
	retryType := atomizer.Classify(retryStep)
	// Only trust a POSITIVE reclassification. The failure-reasoning model
	// often drops the leading phrasing a classifier needs ("run the
	// command: X" becomes just "X"), and Classify then says "general" -
	// which used to silently downgrade a real retry into a chat reply that
	// never ran anything. Absence of a match is not evidence the retry
	// became conversational.
	if retryType == "general" && subtask.Type != "general" {
		retryType = subtask.Type
	}
	if retryType != subtask.Type {
		onLog(fmt.Sprintf("scheduler: retry is a different kind of step (%s -> %s)", subtask.Type, retryType))
	}
	retrySubtask := skills.Subtask{Step: retryStep, Type: retryType}
	rc.Model = s.Models.ForTaskType(retryType)
	retryResult := s.runOnce(skills.Pick(retrySubtask), rc, retrySubtask, onLog)
	retryResult.Meta["retriedAfterFailure"] = true
	retryResult.Meta["originalFailure"] = result.Output
	if retryResult.Verified && (retryType != subtask.Type || retryResult.IsModelReply()) {
		// A retry of another kind, or one that is a model's own words, did something else in the step's place: what it
		// found is reported as tried instead, never as the step done (review T-12).
		onLog(fmt.Sprintf("scheduler: the retry was a different kind of step (%s) - reported as tried instead, not verified", retryType))
		retryResult.Verified = false
		retryResult.Meta["triedInstead"] = retryStep
		retryResult.Output = fmt.Sprintf("The step did not work (%s). Tried instead: %s\n\n%s",
			truncate(strings.TrimSpace(result.Output), 600), retryStep, retryResult.Output)
	}
	if !retryResult.Verified {
		s.recordFailureFeedback(rc.Ctx, subtask.Step, "original: "+result.Output+" | retry ("+retryStep+"): "+retryResult.Output, onLog)
	}
	return retryResult
}

// runDecomposedRecovery runs a real multi-step recovery: the planner breaks
// the failure-reasoning model's plain-language description of what needs to
// happen into concrete steps, and each one runs for real, in order, exactly
// once. No step here gets its own failure-reasoning retry loop - a
// decomposed step that itself fails is recorded and the recovery moves on,
// rather than recursing into reasoning about a failure about a failure.
func (s *Scheduler) runDecomposedRecovery(rc skills.RunContext, subtask skills.Subtask, original skills.Result, reason, description string, planner atomizer.Planner) skills.Result {
	onLog := rc.OnLog
	onLog(fmt.Sprintf("scheduler: decomposing the fix into real steps: %s", description))
	plan := planner.Decompose(rc.Ctx, description)

	if len(plan.Steps) == 0 {
		original.Meta["needsUserInput"] = true
		original.Meta["failureReasoning"] = reason
		s.recordFailureFeedback(rc.Ctx, subtask.Step, original.Output+" | reasoning: "+reason+" | decomposition produced no usable steps", onLog)
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
	failed := -1 // the first recovery step that did not verify
	var failedOutput string
	for i, step := range plan.Steps {
		recoverySubtask := skills.Subtask{Step: step.Text, Type: step.Type}
		onLog(fmt.Sprintf("scheduler: recovery step %d/%d [%s]: %s", i+1, len(plan.Steps), step.Type, step.Text))

		stepRC := rc
		stepRC.Model = s.Models.ForTaskType(step.Type)
		stepResult := s.runOnce(skills.Pick(recoverySubtask), stepRC, recoverySubtask, onLog)

		trail = append(trail, recoveryStepSummary{Step: step.Text, Skill: stepResult.SkillName, Verified: stepResult.Verified})
		last = stepResult
		if approval, _ := stepResult.Meta["needsApproval"].(bool); approval {
			// A recovery step wants to do something consequential. That is
			// the human's call like any other; stop here and ask.
			last.Meta["decomposedRecovery"] = true
			last.Meta["recoveryTrail"] = trail
			return last
		}
		if !stepResult.Verified {
			onLog(fmt.Sprintf("scheduler: recovery step %d/%d failed - continuing with the rest of the recovery plan", i+1, len(plan.Steps)))
			if failed < 0 {
				failed, failedOutput = i, stepResult.Output
			}
		}
	}
	if failed >= 0 && last.Verified {
		// The recovery is verified only when every step of it is: a last step that worked says nothing of one that
		// failed before it (review T-05: the install failed, the version print worked, and the job was "answered").
		last.Verified = false
		last.Output = fmt.Sprintf("The recovery did not work: step %d of %d (%s) failed.\n\n%s\n\nThe last step printed:\n%s",
			failed+1, len(plan.Steps), plan.Steps[failed].Text, strings.TrimSpace(failedOutput), last.Output)
	}

	last.Meta["retriedAfterFailure"] = true
	last.Meta["decomposedRecovery"] = true
	last.Meta["originalFailure"] = original.Output
	last.Meta["recoveryTrail"] = trail
	if !last.Verified {
		s.recordFailureFeedback(rc.Ctx, subtask.Step, "original: "+original.Output+" | decomposed recovery's last step still failed: "+last.Output, onLog)
		return last
	}

	// A real fix, found and proven just now (every step verified) - worth remembering so the same
	// class of failure doesn't need the same research and decomposition all
	// over again. Proposed only, never trusted automatically: this goes to
	// .skills/pending/ and is matched against nothing until a human confirms
	// it.
	if s.SkillLib != nil {
		trigger := fmt.Sprintf("%s (failed because: %s)", subtask.Step, reason)
		proposed, err := s.SkillLib.Propose(trigger, plan.Texts(), "decomposed-recovery")
		if err != nil {
			onLog("skill library: could not propose a skill from this recovery - " + err.Error())
		} else {
			onLog(fmt.Sprintf("skill library: proposed a new skill %q from this recovery - awaiting your confirmation", proposed.Slug))
			last.Meta["proposedSkill"] = proposed.Slug
		}
	}
	return last
}

// matchSkill checks acquired procedural memory before any fresh planning
// happens. Logged explicitly either way - "did it check" and "what came
// back" should never be a silent implementation detail.
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
// explicitly (not silently) so the UI can show that memory was consulted.
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
		// With its date: a correction and the fact it replaced must not read as equally current.
		fact := e.Fact
		if e.CreatedAt > 0 {
			fact += " (noted " + time.Unix(e.CreatedAt, 0).UTC().Format("2006-01-02") + ")"
		}
		facts = append(facts, fact)
	}
	onLog(fmt.Sprintf("memory: recalled %d fact(s) from L1", len(facts)))
	return facts
}

// selfStats is real introspection: exactly the same counts the UI's memory
// drawer shows, given to the model as fact instead of leaving it to guess
// (wrongly) that it has no way to know.
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

// recordFailureFeedback writes one self-generated negative feedback entry -
// "source: self" distinguishes it from feedback a user gave explicitly,
// though both are consulted identically.
func (s *Scheduler) recordFailureFeedback(ctx context.Context, task, detail string, onLog func(string)) {
	if s.Feedback == nil || ctx.Err() != nil {
		return
	}
	if err := s.Feedback.RecordNegative(ctx, task, detail, "self"); err != nil {
		onLog(fmt.Sprintf("feedback: could not record negative feedback: %s", err.Error()))
		return
	}
	onLog("feedback: recorded a negative feedback entry for this failure")
}

var fieldRe = func(field string) *regexp.Regexp {
	return regexp.MustCompile(`(?im)^\**` + field + `\**:\**\s*(.+)$`)
}

var (
	decisionRe = fieldRe("DECISION")
	reasonRe   = fieldRe("REASON")
	retryRe    = fieldRe("RETRY_STEP")
)

// recoveryDecision is what reasonAboutFailure decided to do about a real
// failure - never more than one of these three, and DECOMPOSE only ever
// fires once per original step.
type recoveryDecision int

const (
	recoverAskUser recoveryDecision = iota
	recoverRetry
	recoverDecompose
)

// reasonAboutFailure makes one grounded judgment call about a real failure:
// is the fix a single concrete correction (RETRY), does it genuinely need
// several steps (DECOMPOSE), or is more information from the user required
// (ASK_USER).
//
// DECOMPOSE exists because a single-step retry structurally cannot fix some
// failures. "pip install X" failing with "environment is externally
// managed" got patched over and over with one increasingly elaborate
// compound command ("python3 -m venv v && source v/bin/activate && pip
// install X") - and "source" inside one command has no effect on any
// separate command run afterwards, so it could never work.
func (s *Scheduler) reasonAboutFailure(ctx context.Context, system *llm.Message, subtask skills.Subtask, result skills.Result, pastFeedback []feedback.Entry) (reason, retryStep string, decision recoveryDecision) {
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

Never propose a RETRY_STEP that abandons what was actually asked for something unrelated just because it would exit cleanly. Never propose changing permissions or ownership of directories you do not own - a "Permission denied" on a system path means the path was wrong, not that the system needs fixing. A "successful" command that does something different from what was asked is not a real fix, it's a false success.
No program can be installed system-wide here: there is no sudo password, so sudo, apt, apt-get, snap and dnf always fail. A Python library can be: pip install --user NAME (it stays on this worker). A program that is "not found" (exit code 127) is not installed - retry with one that is (python3 is always here: python3 -c "import urllib.request; r = urllib.request.urlopen('https://example.com'); print(r.status, dict(r.headers))" does what curl would), never with an install.

Failed step: %s
Real failure output:
%s%s

Respond in EXACTLY this format, nothing else:
DECISION: RETRY or DECOMPOSE or ASK_USER
REASON: <one or two sentences>
RETRY_STEP: <the new step (RETRY) or plain description of what needs to happen (DECOMPOSE); leave blank for ASK_USER>`, subtask.Step, truncate(result.Output, 3000), feedbackBlock)

	messages := []llm.Message{{Role: "user", Content: prompt}}
	if system != nil && system.Content != "" {
		messages = append([]llm.Message{*system}, messages...)
	}
	resp, err := s.Provider.Chat(ctx, s.Models.Judgment, messages, llm.Options{TimeoutMs: 120000, Temperature: 0.2})
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
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(m[1]), "*"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
