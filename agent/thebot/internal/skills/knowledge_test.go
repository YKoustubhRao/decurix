package skills

import (
	"context"
	"strings"
	"testing"
)

// GST suite T9 (2026-10-22): calls that quoted real words which did not establish them.

func weighRC(chat *scriptedChat) RunContext {
	return RunContext{Ctx: context.Background(), Provider: chat, Model: "m", OnLog: func(string) {}}
}

func TestACallItsOwnQuoteDoesNotEstablishIsSentBackThenAsked(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "file the GSTR-1", Seen: "Invoices 451 to 458. 455 CANCELLED - Karavali returned it."}
	items := []string{`Documents issued: 0 cancelled | backed by: "Invoices 451 to 458" (the folder)`}
	chat := &scriptedChat{replies: []string{
		`{"items": [{"n": 1, "verdict": "unsure", "why": "the list says nothing of cancellations"}]}`,
		`{"items": [{"n": 1, "verdict": "unsure", "why": "still nothing of cancellations"}]}`,
	}}
	o := &OperatorSkill{}
	nudge, question := o.weighJudgements(weighRC(chat), st, items, "")
	if question != "" || !strings.Contains(nudge, "Not settled yet") || !strings.Contains(nudge, "says nothing of cancellations") ||
		!strings.Contains(nudge, "lookup") || !strings.Contains(nudge, "documentation or a link") {
		t.Fatalf("first: nudge %q question %q", nudge, question)
	}
	nudge, question = o.weighJudgements(weighRC(chat), st, items, "")
	if nudge != "" || !strings.Contains(question, "1. Documents issued: 0 cancelled") {
		t.Fatalf("second: nudge %q question %q", nudge, question)
	}
}

func TestACallBackedOnlyByAWebPageIsPutToThePersonAsASource(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "file the GSTR-1", Seen: "invoices"}
	st.noteLookup(`lookup "GSTR-1 documents issued cancelled invoices"`,
		"https://example.org/gstr1-help\nCancelled invoices are reported in Table 13 in the cancelled column and counted in the total.")
	items := []string{`Counted bill 455 as cancelled | backed by: "reported in Table 13 in the cancelled column" (https://example.org/gstr1-help)`}
	nudge, question := (&OperatorSkill{}).weighJudgements(weighRC(&scriptedChat{}), st, items, "")
	if nudge != "" || !strings.Contains(question, "Is each a source you trust") || !strings.Contains(question, "https://example.org/gstr1-help") {
		t.Fatalf("nudge %q question %q", nudge, question)
	}
	// Their yes makes the page a source for the rest of the goal; a no leaves it out.
	st.settleAskedSources("yes, that is the official help")
	if !strings.Contains(st.Seen, "Table 13") || len(st.AskedSources) != 0 {
		t.Error("a vouched page did not become a source")
	}
	other := &operatorState{AskedSources: []int{0}, Lookups: st.Lookups}
	other.settleAskedSources("no, use our rules")
	if strings.Contains(other.Seen, "Table 13") {
		t.Error("a refused page became a source")
	}
}

func TestAPageThePersonLinkedIsASource(t *testing.T) {
	st := &operatorState{}
	st.notePersonLinks("Follow https://example.org/office-rules.pdf for how we treat rent.")
	if !st.fetchesPersonLink(`lookup "https://example.org/office-rules.pdf"`) {
		t.Error("the person's own link was not taken as theirs")
	}
	if st.fetchesPersonLink(`lookup "GST rent reverse charge"`) {
		t.Error("a search the person did not give was taken as theirs")
	}
}

func TestTheAnswerAboutTheCallsIsAppliedBeforeAnythingElse(t *testing.T) {
	st := &operatorState{Hosted: true, JudgementQuestion: true, UnsureSent: true, JudgedAt: 12, CommandsSucceeded: 12,
		JudgementRounds: 1}
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, HumanReply: "A cancelled invoice still counts in documents issued."}
	st, blocked := (&OperatorSkill{}).resume(rc, Subtask{Step: "file it"}, st)
	if blocked != "" {
		t.Fatal(blocked)
	}
	last := st.Messages[len(st.Messages)-1].Content
	if !strings.Contains(last, "A cancelled invoice still counts") || !strings.Contains(last, "Apply it before anything else") {
		t.Errorf("message: %q", last)
	}
	if st.JudgementQuestion || st.UnsureSent || !st.judgementsDue() {
		t.Error("the calls are not weighed again after the answer")
	}
	if !strings.Contains(st.Seen, "still counts in documents issued") {
		t.Error("the answer is not a source")
	}
}

func TestASiteNothingGivenOrTaughtNamesIsNotOpenedFromMemory(t *testing.T) {
	st := &operatorState{Request: "File Udupi Agro Traders' September GSTR-1 on the GST portal.", Seen: "CLIENT_MASTER: udupiagro"}
	if n := unknownSiteNudge(st, `gui open "https://services.gst.gov.in/services/login"`); !strings.Contains(n, "services.gst.gov.in") ||
		!strings.Contains(n, "gui skills") {
		t.Fatalf("nudge: %q", n)
	}
	st.Seen += "\ngstr1-prepare: runs at http://host.docker.internal:8703/login"
	if n := unknownSiteNudge(st, "gui open http://host.docker.internal:8703/login"); n != "" {
		t.Errorf("a taught address was refused: %q", n)
	}
	research := &operatorState{Request: "Look up the GST council's latest notification online."}
	if n := unknownSiteNudge(research, "gui open https://gstcouncil.gov.in"); n != "" {
		t.Errorf("a research request was refused: %q", n)
	}
	linked := &operatorState{Request: "Read https://example.org/rules.pdf and apply it."}
	linked.notePersonLinks(linked.Request)
	if n := unknownSiteNudge(linked, "gui open https://example.org/rules.pdf"); n != "" {
		t.Errorf("the person's own link was refused: %q", n)
	}
}
