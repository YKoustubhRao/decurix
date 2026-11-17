package eval

// Tests for the scripted expert's answers to confirmation questions (confirm.go).

import (
	"os"
	"strings"
	"testing"
)

func confirmCase(confirms bool) Case {
	return Case{ConfirmsPlans: confirms, Facts: []Fact{
		{ID: "qty", About: "quantity of chairs", Answer: "We ordered 6 chairs.", RefersTo: []string{"qty"}},
		{ID: "rule", About: "the due date rule", Answer: "The due date comes from the bill.", RefersTo: []string{"due date"}},
	}}
}

const plan = PlanQuestionOpening + "\n- The record, read from its document and checked against it before the save: " +
	`Supplier = "Apex Office"; Supplier Invoice No = "AOS-OCT-140"` + "\n" +
	`- Item: "Ergonomic Office Chair"` + "\n" + `- Accepted Qty: "6"` + "\n" + `- Rate (INR): "6,000.00"` +
	"\nReply `yes` to go ahead, or say what to change."

// A plan whose values contradict no fact is confirmed; one that shows another figure than a fact states is corrected,
// naming the field and the fact's own words.
func TestAPlanIsConfirmedOrCorrectedByTheFacts(t *testing.T) {
	if reply, kind := confirmReply(confirmCase(true), plan); reply != PlanYes || kind != "plan" {
		t.Fatalf("a plan that matches the facts: %q %q", reply, kind)
	}
	wrong := strings.Replace(plan, `Accepted Qty: "6"`, `Accepted Qty: "5"`, 1)
	reply, _ := confirmReply(confirmCase(true), wrong)
	if !strings.HasPrefix(reply, "No - Accepted Qty should be as I said: We ordered 6 chairs.") {
		t.Fatalf("a plan against a fact: %q", reply)
	}
}

// Without the suite's leave, a plan is any question (no fact gives leave to act); "is it done?" always gets the
// neutral not-confirmed reply, never a yes.
func TestOnlyASuiteThatConfirmsPlansDoesSo(t *testing.T) {
	if reply, _ := confirmReply(confirmCase(false), plan); reply != "" {
		t.Fatalf("a plan answered without the suite's leave: %q", reply)
	}
	done := "I cannot confirm this is done.\n\nLook at the screen. Reply `done` if it is done, `retry` to have it tried again"
	if reply, kind := confirmReply(confirmCase(false), done); reply != NotConfirmed || kind != "done" {
		t.Fatalf("is-it-done: %q %q", reply, kind)
	}
	if reply, _ := confirmReply(confirmCase(true), "Which ledger should this go to?"); reply != "" {
		t.Fatalf("an ordinary question answered as a confirmation: %q", reply)
	}
}

// The words the expert recognises and sends are the agent's own: kept the same on both sides.
func TestTheConfirmationWordsAreTheAgents(t *testing.T) {
	for file, want := range map[string]string{
		"../../agent/decurix_agent/thebot_harness.py": `PLAN_QUESTION = "` + PlanQuestionOpening + `"`,
		"../../agent/decurix_agent/skill_task.py":     "Reply `done` if it is done, `retry` ",
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s no longer holds %q", file, want)
		}
	}
	// The expert's own "cannot check" reply is read by the agent like any person's reply: the harness holds no copy of
	// it (review of freeze 2, B).
	b, err := os.ReadFile("../../agent/decurix_agent/final_gate.py")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), NotConfirmed) {
		t.Fatalf("final_gate.py holds the expert's reply %q", NotConfirmed)
	}
}

// A date-order question is answered from the case's own fact of how that side writes dates, by the question's
// structure (its mark and the side it names): "1" for day first, "2" for month first. With no such fact it is no
// confirmation, and the expert answers it as any question (owner's ruling 2026-11-09).
func TestTheExpertAnswersADateOrderQuestionFromTheCasesFact(t *testing.T) {
	ask := func(side string) string {
		return "Before I save: " + side + " 04-11-2026, " + DateOrderMark + " 4 November 2026 (day first), or 2 if it is " +
			"11 April 2026 (month first)."
	}
	c := Case{DateOrders: map[string]string{"app": "day", "source": "month"}}
	if reply, kind := confirmReply(c, ask("the application writes the date")); reply != "1" || kind != "date" {
		t.Fatalf("app side: %q %q", reply, kind)
	}
	if reply, _ := confirmReply(c, ask("the document writes the date")); reply != "2" {
		t.Fatalf("source side: %q", reply)
	}
	if reply, _ := confirmReply(Case{}, ask("the application writes the date")); reply != "" {
		t.Fatalf("answered with no fact: %q", reply)
	}
}

// The date-order question's words are the agent's own (task_context.date_question_text).
func TestTheDateOrderWordsAreTheAgents(t *testing.T) {
	b, err := os.ReadFile("../../agent/decurix_agent/task_context.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"which can be read two ways. Reply 1 if it is", `"the document" if question["side"] == "source" else "the application"`, "writes the date"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("task_context.py no longer holds %q", want)
		}
	}
}

// A question about entries picked from an application's lists is confirmed when every entry it lists is a value of a
// record the case expects to be made, and is no confirmation (answered as any question) when one is not.
func TestTheExpertConfirmsPicksTheExpectedRecordsHold(t *testing.T) {
	c := Case{Expected: Expected{Graders: []Grader{
		{Kind: "mock_records", Records: []byte(`[{"number": "1062", "party": "Udyavara Apartments"}, {"party": "Pai Textiles"}]`)},
		{Kind: "absent", Records: []byte(`[{"party": "Suvarna Lifts & Escalators"}]`)},
	}}}
	ask := func(entries ...string) string {
		q := "Before I report this, " + PicksQuestionMark + " - nothing the request, its files or you gave states these for their fields:"
		for _, e := range entries {
			q += "\n- Party A/c name: " + e + " (for 29ABEPP4410Q1Z5)\n  where the documents show it: \"" + e + "\" (1063.docx)"
		}
		return q + "\nReply yes if each is right for the record it is on, or say which entry each should be."
	}
	if reply, kind := confirmReply(c, ask("udyavara  apartments", "Pai Textiles")); reply != PicksYes || kind != "picks" {
		t.Fatalf("expected entries: %q %q", reply, kind)
	}
	if reply, _ := confirmReply(c, ask("Pai Textiles", "Suvarna Lifts & Escalators")); reply != "" {
		t.Fatalf("an entry only an absent grader names was confirmed: %q", reply)
	}
	if reply, _ := confirmReply(Case{}, ask("Pai Textiles")); reply != "" {
		t.Fatalf("confirmed with no expected records: %q", reply)
	}
}

// The picks question's words are the agent's own (final_gate._ask_about_picks).
func TestThePicksQuestionWordsAreTheAgents(t *testing.T) {
	b, err := os.ReadFile("../../agent/decurix_agent/final_gate.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{PicksQuestionMark, `lines.append(f"- {e['label']}: {e['value']}" + (f" (for `, `" (on a record that names no party`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("final_gate.py no longer holds %q", want)
		}
	}
}

// A worker asking in words to press a control the case approves gets the yes the gate's card would get; a question that
// asks to press anything the case does not approve, or asks to press nothing, is no confirmation (answered as any
// question). Found live (2026-11-10, freeze 2.2): "I need approval before clicking "Save to portal" for Sahyadri Lift
// Services' ..." got "I don't know", a non-yes the gate then held the client's own link on.
func TestTheExpertApprovesAWordedAskToPressAnApprovedControl(t *testing.T) {
	c := Case{Approves: []Approval{{Control: "Save to portal"}, {Control: "File with EVC"}, {Control: "Verify"}}}
	live := `I need approval before clicking "Save to portal" for Sahyadri Lift Services' January 2027 GSTR-1 (the imported B2B, B2CS and HSN rows). May I go ahead?`
	if reply, kind := confirmReply(c, live); reply != ApprovalYes || kind != "approval" {
		t.Fatalf("the live question: %q %q", reply, kind)
	}
	if reply, _ := confirmReply(c, `May I click "File with EVC" and then press "Verify"?`); reply != ApprovalYes {
		t.Fatalf("two approved presses: %q", reply)
	}
	for _, q := range []string{
		`May I click "Save to portal" and then "Delete return"? I will press "Delete return" first.`,
		`Should I click "Reset" before importing?`,
		`Which company covers April 2027: "Suvarna Lift Systems 2025-26" or the 2027-28 one?`,
		`Is "Save to portal" the right button?`,
	} {
		if reply, _ := confirmReply(c, q); reply != "" {
			t.Fatalf("answered %q with %q", q, reply)
		}
	}
	if reply, _ := confirmReply(Case{}, live); reply != "" {
		t.Fatalf("a case that approves nothing answered: %q", reply)
	}
}

// The live 2.3 question that names Verify without quotes is an approval ask; an unapproved quoted press still vetoes it.
func TestTheExpertApprovesAnUnquotedAskToPressAnApprovedControl(t *testing.T) {
	c := Case{Approves: []Approval{{Control: "Save to portal"}, {Control: "File with EVC"}, {Control: "Verify"}}}
	live := "I am about to enter the OTP [withheld] and click Verify to complete the filing of the January 2027 GSTR-1. May I proceed?"
	if reply, kind := confirmReply(c, live); reply != ApprovalYes || kind != "approval" {
		t.Fatalf("the live question: %q %q", reply, kind)
	}
	if reply, _ := confirmReply(c, `I will click Verify and then press "Delete return".`); reply != "" {
		t.Fatalf("an unapproved quoted press was let through: %q", reply)
	}
	if reply, _ := confirmReply(c, "Did the Verify step finish?"); reply != "" {
		t.Fatalf("a question that only names Verify was answered: %q", reply)
	}
}

func TestAnApprovalThatAlsoAsksForAValueGetsBoth(t *testing.T) {
	yes := true
	c := Case{Approves: []Approval{{Control: "Verify"}},
		Facts: []Fact{{ID: "otp", About: "the one-time password", Answer: "The OTP is 604217.", RefersTo: []string{"otp"}, Required: &yes}}}
	if got := approvalWithFact(c, "otp"); got != ApprovalYes+" The OTP is 604217." {
		t.Fatalf("the yes and the value: %q", got)
	}
	if got := approvalWithFact(c, ""); got != ApprovalYes {
		t.Fatalf("a question no fact answers gets the yes alone: %q", got)
	}
}
