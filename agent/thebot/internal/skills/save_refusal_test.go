package skills

import (
	"strings"
	"testing"
)

// The refusal the live run met (2026-11-08), as the gui command printed it.
const notesRefusal = "gui: Not pressed: the form does not hold what record B-7 says - Notes holds 'Parcel pickups, 18 " +
	"runs: 2,160.00; Toll charges: 115.00; Taxable val' where the record says 'Parcel pickups, 18 " +
	"runs: 2,160.00; Toll charges: 115.00; Taxable value: 3,600.00'. Correct the field (gui type REF " +
	"\"value\"), then save again."

func refused(output string) shellResult { return shellResult{ExitCode: 1, Output: output} }

func TestTheSameSaveRefusalOnAnUnchangedFormEndsInAQuestion(t *testing.T) {
	st := &operatorState{}
	st.noteSaveRefusal("gui click e16", refused(notesRefusal+" (on e16)"))
	// Commands between the tries change nothing the check reads: a field typed again, a look, a fetch.
	st.noteSaveRefusal("gui type e15 'Parcel pickups'", shellResult{ExitCode: 0, Output: "Typed into e15"})
	st.noteSaveRefusal("curl -s http://app.example.test/bills/new", shellResult{ExitCode: 0, Output: "<html>"})
	// A new listing gives the same button another ref: still the same refusal.
	st.noteSaveRefusal("gui click e31", refused(notesRefusal+" (on e31)"))
	if st.SaveRefusals != 2 || st.SaveRefusalAsk != "" {
		t.Fatalf("after two: %d, asked %q", st.SaveRefusals, st.SaveRefusalAsk)
	}
	st.noteSaveRefusal("gui click e15 && gui key end && gui click e16",
		refused("Clicked e15\nPressed end\n"+notesRefusal+" (on e16)"))
	q := st.SaveRefusalAsk
	if q == "" {
		t.Fatal("not asked after the same refusal three times")
	}
	for _, want := range []string{"3 times now", "Not pressed: the form does not hold what record B-7 says",
		"The field it names: Notes.", "what Notes should hold"} {
		if !strings.Contains(q, want) {
			t.Errorf("the question lacks %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "gui: Not pressed") {
		t.Errorf("the question quotes the tool's prefix: %q", q)
	}
}

func TestAnotherRefusalOrAChangedFormStartsTheCountAgain(t *testing.T) {
	st := &operatorState{}
	st.noteSaveRefusal("gui click e16", refused(notesRefusal))
	st.noteSaveRefusal("gui click e16", refused(notesRefusal))
	// The field typed otherwise: the refusal quotes what it holds now - another refusal.
	changed := strings.Replace(notesRefusal, "Taxable val'", "Taxable value: 3,600.00; CGST'", 1)
	st.noteSaveRefusal("gui click e16", refused(changed))
	st.noteSaveRefusal("gui click e16", refused(changed))
	if st.SaveRefusalAsk != "" {
		t.Fatalf("asked over refusals of a form that changed: %q", st.SaveRefusalAsk)
	}
	st.noteSaveRefusal("gui click e16", refused(changed))
	if st.SaveRefusalAsk == "" {
		t.Fatal("the changed form's refusal, three times, was not asked about")
	}
}

func TestOnlyTheSaveChecksRefusalIsCounted(t *testing.T) {
	for _, r := range []struct {
		command string
		res     shellResult
	}{
		{"gui click e16", shellResult{ExitCode: 0, Output: "Clicked \"Save bill\" (e16). What changed: page is now /bills/7"}},
		{"gui click e16", refused("gui: Not pressed yet - \"Bill\" would be sent holding:\n  Notes: x\nCheck every value")},
		{"gui click e16", shellResult{ExitCode: 3, Output: "gui: " + strings.TrimPrefix(notesRefusal, "gui: ")}},
		{"gui record B-7 --from bill.pdf --fields -", refused("gui: not declared - these values stand neither in the source")},
		{"cat notes.txt", refused(notesRefusal)}, // (not a gui action)
	} {
		st := &operatorState{}
		for i := 0; i < maxSameSaveRefusals+1; i++ {
			st.noteSaveRefusal(r.command, r.res)
		}
		if st.SaveRefusalAsk != "" || st.SaveRefusals != 0 {
			t.Errorf("%s (exit %d) counted as a save refusal", r.command, r.res.ExitCode)
		}
	}
}

func TestTheOperatorAsksInsteadOfTryingTheSameRefusedSaveAgain(t *testing.T) {
	refuse := `gui() { printf '%s\n' "` + strings.ReplaceAll(notesRefusal, `"`, `\"`) + `"; return 1; }; gui click e16`
	chat := &scriptedChat{replies: []string{
		"COMMAND:\n" + refuse,
		"COMMAND:\necho looked",
		"COMMAND:\n" + refuse,
		"COMMAND:\n" + refuse,
		"COMMAND:\n" + refuse, // never run: the person is asked first
		"DONE:\nSaved.",
	}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "enter the bill", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.Meta["needsUserInput"] != true {
		t.Fatalf("not a question: %+v", res)
	}
	q, _ := res.Meta["question"].(string)
	if !strings.Contains(q, "The field it names: Notes.") || !strings.Contains(q, "with the form unchanged") {
		t.Fatalf("question = %q", q)
	}
	if len(chat.calls) != 4 {
		t.Fatalf("the model was asked %d times; the loop should end after the third refusal", len(chat.calls))
	}
	if res.Checkpoint == nil {
		t.Fatal("the goal was not kept for the person's answer")
	}
}

// A save refused because it waits on the agent's own question to the person (core.PERSON_ASK_MARKER), as the gui
// command printed it in the live run of 2026-11-10.
const dateAskRefusal = "gui: Not pressed: the form does not hold what record B-2 says - Date (DD-MM-YYYY) holds " +
	"'02-04-2027' and the record says '02-04-2027', and which day '02-04-2027' is cannot be told from here - this needs " +
	"the person.\nASKS_PERSON: Before I save: the document writes the date 02-04-2027, which can be read two ways. " +
	"Reply 1 if it is 2 April 2027 (day first), or 2 if it is 4 February 2027 (month first)."

// TestASaveWaitingOnTheAgentsQuestionAsksAtOnce: found live (2026-11-10) - the date question waited for the worker's
// next HELP, which never came; the first such refusal now asks, with the agent's own question.
func TestASaveWaitingOnTheAgentsQuestionAsksAtOnce(t *testing.T) {
	st := &operatorState{}
	st.noteSaveRefusal("gui click e16", refused(dateAskRefusal))
	q := st.SaveRefusalAsk
	if !strings.HasPrefix(q, "Before I save: the document writes the date 02-04-2027") ||
		!strings.Contains(q, "which can be read two ways. Reply 1 if it is") {
		t.Fatalf("not asked at once with the agent's question: %q", q)
	}
	if st.SaveRefusals != 0 {
		t.Errorf("counted as a repeat: %d", st.SaveRefusals)
	}
	// The marker counts only on a gui action the save check refused.
	for _, r := range []struct {
		command string
		res     shellResult
	}{
		{"cat notes.txt", refused(dateAskRefusal)},
		{"gui click e16", shellResult{ExitCode: 0, Output: dateAskRefusal}},
		{"gui click e16", refused("gui: something else\nASKS_PERSON: Before I save: ...")},
	} {
		st := &operatorState{}
		st.noteSaveRefusal(r.command, r.res)
		if st.SaveRefusalAsk != "" {
			t.Errorf("%s (exit %d) asked: %q", r.command, r.res.ExitCode, st.SaveRefusalAsk)
		}
	}
}

func TestTheOperatorAsksTheAgentsQuestionOnTheFirstRefusal(t *testing.T) {
	refuse := `gui() { printf '%s\n' "` + strings.ReplaceAll(dateAskRefusal, `"`, `\"`) + `"; return 1; }; gui click e16`
	chat := &scriptedChat{replies: []string{
		"COMMAND:\n" + refuse,
		"COMMAND:\necho went round the save", // never run: the person is asked first
		"DONE:\nSaved.",
	}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "enter the bill", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.Meta["needsUserInput"] != true {
		t.Fatalf("not a question: %+v", res)
	}
	if q, _ := res.Meta["question"].(string); !strings.Contains(q, "Reply 1 if it is 2 April 2027 (day first)") {
		t.Fatalf("question = %q", q)
	}
	if len(chat.calls) != 1 {
		t.Fatalf("the model was asked %d times; the loop should ask the person after the first refusal", len(chat.calls))
	}
	if res.Checkpoint == nil {
		t.Fatal("the goal was not kept for the person's answer")
	}
}

// TestARefusedMadeUpValueNamesItsField: the agent's refusal of a value that looks made up names its field as
// "any source: UQC: 'NA'"; the person is asked about UQC, and a refusal naming no field never says "the field it
// names: the field it names" (found live 2026-11-09).
func TestARefusedMadeUpValueNamesItsField(t *testing.T) {
	q := saveRefusalQuestion("Not pressed: these values look invented - a filler, not a value read from any source: "+
		"UQC: 'NA'. Never type a value you did not read from a source", 3)
	if !strings.Contains(q, "The field it names: UQC.") || !strings.Contains(q, "what UQC should hold") {
		t.Fatalf("question = %q", q)
	}
	q = saveRefusalQuestion("Not pressed: something else entirely", 3)
	if strings.Contains(q, "field it names") || !strings.Contains(q, "Tell me how to go on") {
		t.Fatalf("question = %q", q)
	}
}
