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
