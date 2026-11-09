// How the scripted expert answers a question that only asks the person to confirm the worker's own reading: a taught
// run's plan, and "is it done?" (owner's decision, 2026-11-08). Before, both got DontKnow - no fact gives leave to act
// - and the worker read that as a change to its plan (so a taught run never replayed) or as a reason to try again (a
// correct result went round for 50 minutes).
package eval

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// PlanQuestionOpening is how a taught worker opens the plan it puts to the person before its first replays (the
// agent's thebot_harness.PLAN_QUESTION; TestThePlanOpeningIsTheAgents keeps the two the same).
const PlanQuestionOpening = "Before I change anything, this is how I will do it:"

// DoneQuestionMark is what the worker's "I cannot confirm this is done" question asks the person to reply (the agent's
// skill_task._doubt_text).
const DoneQuestionMark = "Reply `done` if it is done, `retry`"

// PlanYes is the expert's yes to a plan whose values contradict none of the case's facts.
const PlanYes = "Yes, go ahead."

// NotConfirmed is the expert's reply to "is it done?": it cannot check, so the result stands as reported, unchecked -
// the graders decide. The agent reads it as "leave it" (final_gate.NOT_CONFIRMED_REPLY), never as done or a retry.
const NotConfirmed = "I can't check that from here - report it as you see it, not confirmed."

// planPairRe is a value a plan shows: `Label = "value"` or `Label: "value"` (the agent's _record_plan_text).
var planPairRe = regexp.MustCompile(`([A-Za-z][^=;:\n"]{0,60}?)\s*[=:]\s*"([^"\n]*)"`)

// confirmReply is the expert's reply to a confirmation question, with its kind ("plan" or "done"), or "" for any
// other question. A plan is confirmed only when the suite says the person does (Case.ConfirmsPlans).
func confirmReply(c Case, question string) (string, string) {
	q := strings.TrimSpace(question)
	if strings.Contains(q, DoneQuestionMark) {
		return NotConfirmed, "done"
	}
	if c.ConfirmsPlans && strings.HasPrefix(q, PlanQuestionOpening) {
		return planReply(c, q), "plan"
	}
	return "", ""
}

// planReply is the answer of a person who glances at the plan: each value it shows is set against the case's facts
// that name its field (by their refers_to, as a question names a fact); a fact that states one figure, which the
// plan's value does not hold, is a correction - "No - <field> should be as I said: <the fact's words>". Decided by
// structure: label and value pairs, and figures compared as amounts.
func planReply(c Case, plan string) string {
	for _, m := range planPairRe.FindAllStringSubmatch(plan, -1) {
		label := strings.TrimSpace(strings.TrimLeft(m[1], "- "))
		shown := numbersIn(m[2])
		if label == "" || len(shown) == 0 {
			continue
		}
		for _, f := range c.FactsNamedBy(label) {
			said := numbersIn(f.Answer)
			if len(said) != 1 {
				continue // a fact of no figure, or of several, says nothing one value can contradict
			}
			if !holdsRat(shown, said[0]) {
				return fmt.Sprintf("No - %s should be as I said: %s", label, f.Answer)
			}
		}
	}
	return PlanYes
}

// holdsRat reports whether want is one of have.
func holdsRat(have []*big.Rat, want *big.Rat) bool {
	for _, h := range have {
		if h.Cmp(want) == 0 {
			return true
		}
	}
	return false
}
