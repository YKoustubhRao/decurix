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
		"../../agent/decurix_agent/final_gate.py":     `NOT_CONFIRMED_REPLY = "` + NotConfirmed + `"`,
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s no longer holds %q", file, want)
		}
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
