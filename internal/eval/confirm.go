// How the scripted expert answers a question that only asks the person to confirm the worker's own reading: a taught
// run's plan, and "is it done?" (owner's decision, 2026-11-08). Before, both got DontKnow - no fact gives leave to act
// - and the worker read that as a change to its plan (so a taught run never replayed) or as a reason to try again (a
// correct result went round for 50 minutes).
package eval

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// PicksQuestionMark is what the agent's question about entries picked from an application's lists says
// (final_gate._ask_about_picks).
const PicksQuestionMark = "please confirm the entries I chose from the application's own lists"

// PicksYes is the expert's yes to picked entries that the case's expected records all hold.
const PicksYes = "yes"

// pressObjectRe is the control a question asks to press: the quoted label right after a press verb.
var pressObjectRe = regexp.MustCompile(`(?i)\b(?:click(?:ing)?|press(?:ing)?|tap(?:ping)?|submit(?:ting)?|approv(?:e|al) (?:to|for|before)(?: clicking| pressing)?)\s+(?:on\s+)?(?:the\s+)?["“'‘]([^"”'’\n]{1,79})["”'’]`)

// approvalReply is the answer of a person asked in words to OK a press: "Yes, go ahead." when the question asks to
// press a control the case approves (Case.Approves) and asks to press no other; "" otherwise (classified as any
// question). A person who approves "Save to portal" when the gate's card asks would say yes when the worker asks in
// words too. Found live (2026-11-10, freeze 2.2): the worker asked "I need approval before clicking "Save to portal"
// for <client> ...", the expert's "I don't know" was a non-yes, and the gate then held the client's own link named in
// that question; the gate side is narrowed separately (core._person_hold), this answers the question as a person would.
func approvalReply(c Case, question string) string {
	if len(c.Approves) == 0 {
		return ""
	}
	approved := map[string]bool{}
	for _, a := range c.Approves {
		approved[normValue(a.Control)] = true
	}
	asked := pressObjectRe.FindAllStringSubmatch(question, -1)
	if len(asked) == 0 {
		return ""
	}
	for _, m := range asked {
		if !approved[normValue(m[1])] {
			return ""
		}
	}
	return ApprovalYes
}

// pickLineRe is one entry the picks question lists: "- <field>: <entry>" with an optional "(for ...)" or "(on a record
// ...)" after it.
var pickLineRe = regexp.MustCompile(`(?m)^- ([^:\n]+): (.+?)(?: \((?:for |on a record )[^\n]*\))?\s*$`)

// picksReply is the answer of a person who knows their own records: "yes" when every entry the question lists is a
// value of a record the case expects to be made (its mock_records graders); "" otherwise (the question is classified
// as any other). A person knows their customers' ledgers and their places of supply; the graders still decide whether
// the work is right. Found live (2026-11-10, books curve): every customer picked from the ledger list was put to the
// person, and the expert's "I don't know" kept nothing for the next month.
func picksReply(c Case, question string) string {
	want := expectedValues(c)
	lines := pickLineRe.FindAllStringSubmatch(question, -1)
	if len(lines) == 0 || len(want) == 0 {
		return ""
	}
	for _, m := range lines {
		if !want[normValue(m[2])] {
			return ""
		}
	}
	return PicksYes
}

// expectedValues is every text value of the records the case's mock_records graders expect, normalized.
func expectedValues(c Case) map[string]bool {
	out := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if n := normValue(x); n != "" {
				out[n] = true
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, g := range c.Expected.Graders {
		if g.Kind != "mock_records" || len(g.Records) == 0 {
			continue
		}
		var recs any
		if json.Unmarshal(g.Records, &recs) == nil {
			walk(recs)
		}
	}
	return out
}

// normValue is a value as compared: spaces collapsed, case folded.
func normValue(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

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

// DateOrderMark is what the agent's date-order question says (task_context.date_question_text): which of two
// readings a digit date is, the day-first one numbered 1 and the month-first one 2.
const DateOrderMark = "which can be read two ways. Reply 1 if it is"

// dateOrderSides is how the date-order question names the side it asks about (task_context.date_question_text).
var dateOrderSides = map[string]string{"the application writes the date": "app", "the document writes the date": "source"}

// confirmReply is the expert's reply to a confirmation question, with its kind ("plan", "done", "date", "picks" or "approval"), or "" for
// any other question. A plan is confirmed only when the suite says the person does (Case.ConfirmsPlans); a date-order
// question is answered from the case's own fact of how that side writes dates (Case.DateOrders) - a person knows how
// their own application writes dates (owner's ruling 2026-11-09: unanswered, every save of a month asked it again).
func confirmReply(c Case, question string) (string, string) {
	q := strings.TrimSpace(question)
	if strings.Contains(q, DoneQuestionMark) {
		return NotConfirmed, "done"
	}
	if strings.Contains(q, DateOrderMark) {
		for said, side := range dateOrderSides {
			if !strings.Contains(q, said) {
				continue
			}
			switch c.DateOrders[side] {
			case "day":
				return "1", "date"
			case "month":
				return "2", "date"
			}
		}
		return "", ""
	}
	if c.ConfirmsPlans && strings.HasPrefix(q, PlanQuestionOpening) {
		return planReply(c, q), "plan"
	}
	if strings.Contains(q, PicksQuestionMark) {
		if yes := picksReply(c, q); yes != "" {
			return yes, "picks"
		}
		return "", ""
	}
	if yes := approvalReply(c, q); yes != "" {
		return yes, "approval"
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
