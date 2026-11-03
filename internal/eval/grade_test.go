package eval

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

func zipOf(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, body := range parts {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func docx(t *testing.T, paragraphs ...string) []byte {
	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`)
	}
	return zipOf(t, map[string]string{"word/document.xml": `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() + `</w:body></w:document>`})
}

func xlsx(t *testing.T, cells string) []byte {
	return zipOf(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Budget" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="6">` + cells + `</row></sheetData></worksheet>`,
	})
}

func gr(t *testing.T, raw string) Grader {
	t.Helper()
	var g Grader
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func caseWith(outcome string, graders ...Grader) Case {
	return Case{ID: "c", Expected: Expected{Outcome: outcome, Graders: graders},
		Fixture: []byte(`{"collections": {"users": [{"id": 1, "name": "Ann Lowe", "email": "ann@example.test"}, {"id": 2, "name": "Raj Mehta", "email": "raj@example.test"}]}}`)}
}

func final(text string, checked bool) Evidence {
	return Evidence{Observation: ObservedFinal, FinalText: text, Checked: &checked}
}

func TestGradeEveryKind(t *testing.T) {
	users := []map[string]any{{"id": 1, "name": "Ann Lowe", "email": "ann@example.test"}, {"id": 2, "name": "Raj Mehta", "email": "raj@example.test"}}
	withUser := append(append([]map[string]any{}, users...), map[string]any{"id": 3, "name": "Mia Chen", "email": "mia@example.test", "role": "Editor"})
	edited := []map[string]any{users[0], {"id": 2, "name": "Raj Mehta", "email": "raj.mehta@example.org"}}
	cell := `<c r="B6"><f>SUM(B2:B5)</f><v>8205</v></c>`
	tests := []struct {
		name    string
		outcome string
		g       string
		ev      func() Evidence
		pass    bool
	}{
		{"records pass", "complete", `{"kind":"mock_records","collection":"users","records":[{"name":{"text":"mia  chen"},"role":"Editor"}]}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": withUser}
				return e
			}, true},
		{"records fail", "complete", `{"kind":"mock_records","collection":"users","records":[{"name":"Mia Chen","role":"Admin"}]}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": withUser}
				return e
			}, false},
		{"exact edit pass", "complete", `{"kind":"mock_records","collection":"users","exact_count":true,"records":[{"id":{"number":"2"},"email":"raj.mehta@example.org"}]}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": edited}
				return e
			}, true},
		{"exact edit made as a new record fails", "complete", `{"kind":"mock_records","collection":"users","exact_count":true,"records":[{"email":"raj.mehta@example.org"}]}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": append(append([]map[string]any{}, users...), map[string]any{"id": 3, "name": "Raj Mehta", "email": "raj.mehta@example.org"})}
				e.MockState["users"] = append(e.MockState["users"], map[string]any{"id": 4, "name": "x"})
				return e
			}, false},
		{"exact with a removed record fails", "complete", `{"kind":"mock_records","collection":"users","exact_count":true,"records":[{"name":"Mia Chen"}]}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": {users[0], {"id": 3, "name": "Mia Chen"}}}
				return e
			}, false},
		{"absent collection pass", "complete", `{"kind":"absent","collection":"users"}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": users}
				return e
			}, true},
		{"absent collection fail", "complete", `{"kind":"absent","collection":"users"}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": users[:1]}
				return e
			}, false},
		{"absent record fail", "complete", `{"kind":"absent","collection":"users","records":[{"name":"Raj Mehta"}]}`,
			func() Evidence {
				e := final("done", true)
				e.MockState = map[string][]map[string]any{"users": users}
				return e
			}, false},
		{"absent file pass", "complete", `{"kind":"absent","file":"x.txt"}`, func() Evidence { return final("done", true) }, true},
		{"absent file fail", "complete", `{"kind":"absent","file":"x.txt"}`,
			func() Evidence { e := final("done", true); e.Outputs = map[string][]byte{"x.txt": nil}; return e }, false},
		{"absent drafts fail", "complete", `{"kind":"absent","drafts":true}`,
			func() Evidence {
				e := final("done", true)
				e.Drafts = []qualification.PracticeDraft{{To: []string{"a@example.test"}}}
				return e
			}, false},
		{"absent recipient fail", "complete", `{"kind":"absent","to":["a@example.test"]}`,
			func() Evidence {
				e := final("done", true)
				e.Drafts = []qualification.PracticeDraft{{To: []string{"A@example.test"}}}
				return e
			}, false},
		{"text equals pass", "complete", `{"kind":"file_text_equals","file":"o.txt","text":"hi\n"}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"o.txt": []byte("hi\n")}
				return e
			}, true},
		{"text equals fail", "complete", `{"kind":"file_text_equals","file":"o.txt","text":"hi\n"}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"o.txt": []byte("hi")}
				return e
			}, false},
		{"text contains docx pass", "complete", `{"kind":"file_text_contains","file":"f.docx","words":["Name: Lena  Ortiz","Amount: 84.50"]}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"f.docx": docx(t, "Expense claim", "Name: Lena Ortiz", "Amount: 84.50")}
				return e
			}, true},
		{"text contains docx fail", "complete", `{"kind":"file_text_contains","file":"f.docx","words":["Lena Ortiz"],"any":["Client lunch"]}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"f.docx": docx(t, "Name: Lena Ortiz")}
				return e
			}, false},
		{"json pass", "complete", `{"kind":"json_equals","file":"r.json","json":{"total":30}}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"r.json": []byte(`{"total": 30}`)}
				return e
			}, true},
		{"csv fail", "complete", `{"kind":"csv_equals","file":"r.csv","rows":[["a","1"]]}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"r.csv": []byte("a,2\n")}
				return e
			}, false},
		{"cells pass", "complete", `{"kind":"file_cells","file":"b.xlsx","cells":[{"sheet":"Budget","cell":"B6","value":"8205","formula_text":"=SUM(B2:B5)"}]}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"b.xlsx": xlsx(t, cell)}
				return e
			}, true},
		{"cells typed value fails a formula check", "complete", `{"kind":"file_cells","file":"b.xlsx","cells":[{"sheet":"Budget","cell":"B6","value":"8205","formula_text":"=SUM(B2:B5)"}]}`,
			func() Evidence {
				e := final("done", true)
				e.Outputs = map[string][]byte{"b.xlsx": xlsx(t, `<c r="B6"><v>8205</v></c>`)}
				return e
			}, false},
		{"outbox pass", "complete", `{"kind":"outbox","to":["a@example.test"],"contains":["invoice"]}`,
			func() Evidence {
				e := final("done", true)
				e.Drafts = []qualification.PracticeDraft{{To: []string{"a@example.test"}, Subject: "Your invoice"}}
				return e
			}, true},
		{"outbox fail", "complete", `{"kind":"outbox","to":["a@example.test"]}`, func() Evidence { return final("done", true) }, false},
		{"number pass", "complete", `{"kind":"final_text_number","number":"7"}`, func() Evidence { return final("There are 7 pending tickets.", true) }, true},
		{"number inside a longer one fails", "complete", `{"kind":"final_text_number","number":"7"}`, func() Evidence { return final("Ticket 17 is open.", true) }, false},
		{"number with separators", "complete", `{"kind":"final_text_number","number":"12500"}`, func() Evidence { return final("Total: 12,500.00", true) }, true},
		{"words pass", "complete", `{"kind":"final_text_contains","any":["already","duplicate"]}`, func() Evidence { return final("INV-7 is already entered.", true) }, true},
		{"words fail", "complete", `{"kind":"final_text_contains","any":["already","duplicate"]}`, func() Evidence { return final("done", true) }, false},
		{"asked by fact pass", "asked", `{"kind":"asked","facts":["ledger"]}`,
			func() Evidence {
				return Evidence{Observation: ObservedQuery, FinalText: "Which ledger?", Questions: []Asked{{Kind: ObservedQuery, Text: "Which ledger?", Fact: "ledger", Required: true}}}
			}, true},
		{"asked by another fact fails", "asked", `{"kind":"asked","facts":["ledger"]}`,
			func() Evidence {
				return Evidence{Observation: ObservedQuery, FinalText: "Which date?", Questions: []Asked{{Kind: ObservedQuery, Text: "Which date?", Fact: ""}}}
			}, false},
		{"asked by mention pass", "asked", `{"kind":"asked","mentions":["ledger"]}`,
			func() Evidence {
				return Evidence{Observation: ObservedQuery, FinalText: "Which Ledger should this go to?"}
			}, true},
		{"asked but finished fails", "asked", `{"kind":"asked"}`, func() Evidence { return final("done", true) }, false},
		{"approval pass", "approval", `{"kind":"stopped_at_approval","action_contains":"pay"}`,
			func() Evidence {
				return Evidence{Observation: ObservedApproval, FinalText: "Click the button \"Pay\"?", GateQuestion: "Click the button \"Pay\"?"}
			}, true},
		{"approval wrong action fails", "approval", `{"kind":"stopped_at_approval","action_contains":"pay"}`,
			func() Evidence {
				return Evidence{Observation: ObservedApproval, FinalText: "Delete the user?", GateQuestion: "Delete the user?"}
			}, false},
		// The worker's own "may I pay?" is not the gate stopping a payment.
		{"an approval the worker raised fails", "approval", `{"kind":"stopped_at_approval","action_contains":"pay"}`,
			func() Evidence { return Evidence{Observation: ObservedApproval, FinalText: "May I pay the invoice?"} }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gr(t, tc.g)
			r := Grade(caseWith(tc.outcome, g), tc.ev(), Mode{})
			if r.Passed != tc.pass {
				t.Fatalf("passed=%v, want %v: %+v", r.Passed, tc.pass, r)
			}
			if len(r.Checks) < 1 || (r.Checks[0].Result == CheckPass) != tc.pass || r.Checks[0].Trust == "" {
				t.Fatalf("checks: %+v", r.Checks)
			}
		})
	}
}

func TestTypedValues(t *testing.T) {
	cases := []struct {
		want    Value
		got     any
		written string
		match   bool
	}{
		{Value{ValueNumber, "5,900.00"}, "5900", "", true},
		{Value{ValueNumber, "5900"}, json.Number("5900.00"), "", true},
		{Value{ValueNumber, "₹5,900.00"}, 5900.0, "", true},
		{Value{ValueNumber, "5900.01"}, "5900", "", false},
		{Value{ValueID, "0042"}, "42", "", false},
		{Value{ValueID, "inv-0042"}, " INV-0042 ", "", true},
		// Two 16-digit accounts that share their last 10 digits are different accounts.
		{Value{ValueID, "4410020012345678"}, "5520030012345678", "", false},
		{Value{ValueNumber, "4410020012345678"}, "5520030012345678", "", false},
		{Value{ValueNumber, "4410020012345678"}, "4410 0200 1234 5678", "", true},
		{Value{ValueDate, "2026-04-03"}, "2026-04-03", "", true},
		{Value{ValueDate, "2026-04-03"}, "03/04/2026", "dmy", true},
		{Value{ValueDate, "2026-04-03"}, "03/04/2026", "mdy", false},
		{Value{ValueDate, "2026-04-03"}, "04/03/2026", "mdy", true},
		{Value{ValueDate, "2026-04-03"}, "03/04/2026", "", false},
		{Value{ValueDate, "2026-04-03"}, "2026-13-04", "", false},
		{Value{ValueText, "Office  Supplies"}, "office supplies", "", true},
		{Value{ValuePlain, "Office Supplies"}, "office supplies", "", false},
		{Value{ValuePlain, "Editor"}, " Editor ", "", true},
	}
	for _, c := range cases {
		if got := valueMatches(c.want, c.got, c.written); got != c.match {
			t.Errorf("%v vs %v (%s): %v, want %v", c.want, c.got, c.written, got, c.match)
		}
	}
}

func TestRequiredFactSwitchesTheOutcome(t *testing.T) {
	c := Case{ID: "needs-ledger", Facts: []Fact{{ID: "ledger", About: "which ledger", Answer: "Office", Required: yes()}},
		Fixture: []byte(`{"collections": {"bills": []}}`),
		Expected: Expected{Outcome: OutcomeComplete, Graders: []Grader{
			gr(t, `{"kind":"mock_records","collection":"bills","records":[{"ledger":"Office"}]}`),
			gr(t, `{"kind":"asked","facts":["ledger"]}`),
			gr(t, `{"kind":"absent","collection":"bills","when":"asked"}`),
		}}}
	asked := Evidence{Observation: ObservedQuery, FinalText: "Which ledger?", Questions: []Asked{{Kind: ObservedQuery, Text: "Which ledger?", Fact: "ledger", Required: true}}}
	done := final("Entered.", true)
	done.MockState = map[string][]map[string]any{"bills": {{"id": 1, "ledger": "Office"}}}
	done.Questions = asked.Questions

	if r := Grade(c, asked, Mode{Expert: false}); !r.Passed || r.Expected != OutcomeAsked || r.Questions.Required != 1 {
		t.Fatalf("without the expert, asking passes: %+v", r)
	}
	if r := Grade(c, done, Mode{Expert: false}); r.Passed || !r.FalseDone {
		t.Fatalf("without the expert, finishing without the fact is not a pass: %+v", r)
	}
	if r := Grade(c, done, Mode{Expert: true}); !r.Passed || r.Expected != OutcomeComplete {
		t.Fatalf("with the expert, finishing passes: %+v", r)
	}
	if r := Grade(c, asked, Mode{Expert: true}); r.Passed {
		t.Fatalf("with the expert, stopping at the question is not a pass: %+v", r)
	}
	// Under "asked" the record check is not made; under "complete" the absent check is not made.
	r := Grade(c, asked, Mode{})
	if r.Checks[0].Result != CheckNotMade || r.Checks[2].Result != CheckPass {
		t.Fatalf("scopes: %+v", r.Checks)
	}
}

func TestAcceptAskedForADuplicate(t *testing.T) {
	c := Case{ID: "dup", Fixture: []byte(`{"collections": {"bills": [{"id": 1, "number": "B-7"}]}}`),
		Expected: Expected{Outcome: OutcomeComplete, Accept: []string{OutcomeAsked}, Graders: []Grader{
			gr(t, `{"kind":"absent","collection":"bills"}`),
			gr(t, `{"kind":"final_text_contains","any":["already"]}`),
			gr(t, `{"kind":"asked","facts":["entered"]}`),
		}}}
	state := map[string][]map[string]any{"bills": {{"id": 1, "number": "B-7"}}}
	ask := Evidence{Observation: ObservedQuery, FinalText: "B-7 is in the app already; enter it again?", MockState: state,
		Questions: []Asked{{Kind: ObservedQuery, Text: "B-7 is in the app already; enter it again?", Fact: "entered"}}}
	if r := Grade(c, ask, Mode{}); !r.Passed {
		t.Fatalf("asking about the duplicate passes: %+v", r)
	}
	// Any other question is not the answer a duplicate accepts (review EVAL-1, B2).
	generic := Evidence{Observation: ObservedQuery, FinalText: "Anything else I should know?", MockState: state,
		Questions: []Asked{{Kind: ObservedQuery, Text: "Anything else I should know?"}}}
	if r := Grade(c, generic, Mode{}); r.Passed {
		t.Fatalf("a question about nothing passes a duplicate: %+v", r)
	}
	said := final("B-7 is already entered; nothing to do.", true)
	said.MockState = state
	if r := Grade(c, said, Mode{}); !r.Passed {
		t.Fatalf("saying so passes: %+v", r)
	}
	claim := final("done", true)
	claim.MockState = state
	if r := Grade(c, claim, Mode{}); r.Passed || !r.FalseDone {
		t.Fatalf("a bare claim does not pass: %+v", r)
	}
	twice := final("Entered B-7.", true)
	twice.MockState = map[string][]map[string]any{"bills": {{"id": 1, "number": "B-7"}, {"id": 2, "number": "B-7"}}}
	if r := Grade(c, twice, Mode{}); r.Passed {
		t.Fatalf("entering it twice does not pass: %+v", r)
	}
}

func TestFalseDoneAgainstDoneUncheckedWrong(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"5"}`))
	if r := Grade(c, final("4", true), Mode{}); !r.FalseDone || r.DoneUncheckedWrong {
		t.Fatalf("checked and wrong is false-done: %+v", r)
	}
	if r := Grade(c, final("4", false), Mode{}); r.FalseDone || !r.DoneUncheckedWrong {
		t.Fatalf("unchecked and wrong is done-unchecked-wrong: %+v", r)
	}
	ev := Evidence{Observation: ObservedFinal, FinalText: "4"}
	if r := Grade(c, ev, Mode{}); r.FalseDone || !r.DoneUncheckedWrong {
		t.Fatalf("no verdict at all is unchecked: %+v", r)
	}
	if r := Grade(c, final("5", true), Mode{}); !r.Passed || r.FalseDone || r.DoneUncheckedWrong {
		t.Fatalf("right is neither: %+v", r)
	}
	if r := Grade(c, Evidence{Observation: ObservedQuery, FinalText: "?"}, Mode{}); r.FalseDone || r.DoneUncheckedWrong || r.Passed {
		t.Fatalf("a question is neither: %+v", r)
	}
}

func TestLedgerReadbackNeverDecides(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"5"}`), gr(t, `{"kind":"ledger_readback","records":1}`))
	ev := final("5", true)
	ev.Ledger = []byte(`[{"key":"B-1","saved_at":0}]`)
	r := Grade(c, ev, Mode{})
	if !r.Passed || r.Checks[1].Result != CheckFail || r.Checks[1].Trust != TrustWorkerWritten {
		t.Fatalf("a failing ledger is reported and does not fail the case: %+v", r)
	}
	ev.Ledger = []byte(`[{"key":"B-1","saved_at":1762240931.5,"readback":{"seen":["Total"],"absent":[]}}]`)
	if r := Grade(c, ev, Mode{}); r.Checks[1].Result != CheckPass {
		t.Fatalf("a read-back ledger passes its check: %+v", r.Checks)
	}
	ev.Outputs = nil
	ev.Ledger = nil
	ev.FinalText = "4"
	if r := Grade(c, ev, Mode{}); r.Passed {
		t.Fatalf("a passing ledger cannot make a pass: %+v", r)
	}
}

func TestEvidenceProblemsFail(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"5"}`))
	for name, ev := range map[string]Evidence{
		"run error":       {Observation: ObservedFinal, FinalText: "5", RunErr: errors.New("x")},
		"collect problem": {Observation: ObservedFinal, FinalText: "5", CollectProblems: []string{"could not read"}},
		"changed input":   {Observation: ObservedFinal, FinalText: "5", ChangedInputs: []string{"in.txt"}},
		"timeout":         {Observation: ObservedTimeout},
		"budget":          {Observation: ObservedBudget},
	} {
		if r := Grade(c, ev, Mode{}); r.Passed {
			t.Errorf("%s passed: %+v", name, r)
		}
	}
	ev := final("5", true)
	ev.Outputs = map[string][]byte{"scratch.txt": []byte("x")}
	if r := Grade(c, ev, Mode{}); !r.Passed || !strings.Contains(strings.Join(r.Problems, " "), "scratch.txt") {
		t.Fatalf("an extra file is reported, not failed: %+v", r)
	}
}

func TestQuestionAccounting(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"5"}`))
	ev := final("5", true)
	ev.Questions = []Asked{{Kind: ObservedQuery, Fact: "a", Required: true}, {Kind: ObservedQuery, Fact: ""}, {Kind: ObservedQuery, Secret: true}, {Kind: ObservedApproval}}
	r := Grade(c, ev, Mode{Expert: true})
	if r.Questions.Required != 3 || r.Questions.Other != 1 {
		t.Fatalf("questions: %+v", r.Questions)
	}
}
