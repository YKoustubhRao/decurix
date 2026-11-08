package eval

// The office suites (made by evals/tools/make_office_suites.py on the ca-office mock) load, and calibrate offline: a
// bare "done" and a question about nothing pass no case, and every case's scripted solution passes its own graders on
// the real mock (form posts under each app's own host name, the portal's sign-in kept in a cookie jar).

import (
	"strings"
	"testing"
)

func TestOfficeBooksV1SuiteLoads(t *testing.T) {
	s := calibrateSuite(t, "office-books-v1", 4, 4)
	checkOfficeSuite(t, s, []string{"Save voucher", "Save ledger"})
	// The bill missing from the mail is the person's to give: no input holds its number, so only asking passes.
	c, _ := s.Case("ob-4-january")
	if f, ok := c.Fact("invoice-1060"); !ok || !f.IsRequired() {
		t.Fatal("ob-4-january must require the missing bill's fact")
	}
	for name, b := range c.Files {
		if strings.Contains(name, "1060") || strings.Contains(string(b), "Lift emergency phone line") {
			t.Errorf("ob-4-january: the input %s gives away the missing bill", name)
		}
	}
	if strings.Contains(string(c.Fixture), "Lift emergency phone line") {
		t.Error("ob-4-january: the fixture gives away the missing bill")
	}
}

func TestOfficeGSTR1BooksV1SuiteLoads(t *testing.T) {
	s := calibrateSuite(t, "office-gstr1-books-v1", 4, 4)
	checkOfficeSuite(t, s, []string{"Save to portal", "File with EVC"})
}

func TestOfficeGSTR1ChatV1SuiteLoads(t *testing.T) {
	s := calibrateSuite(t, "office-gstr1-chat-v1", 4, 4)
	checkOfficeSuite(t, s, []string{"File with EVC"})
}

// TestOfficeTrapsFailTheirGraders runs each office suite's scripted solution with one of a real month's slips put in,
// and the case's graders must fail it: they decide by the trap, not only by the work being there.
func TestOfficeTrapsFailTheirGraders(t *testing.T) {
	py := findPython(t)
	if py == "" {
		t.Skip("no Python 3 here: the traps need the mock")
	}
	root := repoRoot(t)
	slips := []struct {
		suite, caseID, why string
		mutate             func(line string) string
	}{
		{"office-books-v1", "ob-1-october", "a bill goes into the company the books opened on",
			func(l string) string {
				if strings.Contains(l, "number=1043") {
					return strings.Replace(l, "/company/1/", "/company/2/", 1)
				}
				return l
			}},
		{"office-books-v1", "ob-4-january", "a short date takes the period's first January",
			func(l string) string { return strings.Replace(l, "date=27-01-2027", "date=27-1", 1) }},
		{"office-books-v1", "ob-3-december", "the bill sent twice is entered twice",
			func(l string) string {
				if strings.Contains(l, "number=1056") {
					return l + "\n" + l
				}
				return l
			}},
		{"office-gstr1-books-v1", "og-1-october", "the return is exported into the folder the dialog was left on",
			func(l string) string {
				return strings.ReplaceAll(l, "GST Returns/Sahyadri Lift Services/2026-27/7. OCTOBER", "GST Returns/Kalinga Hardware/2026-27/7. OCTOBER")
			}},
		{"office-gstr1-chat-v1", "oc-3-december", "the HSN summary leaves out the inter-state invoices' IGST",
			func(l string) string { return strings.Replace(l, "iamt=45000.00", "iamt=0", 1) }},
		{"office-gstr1-chat-v1", "oc-2-november", "the cancelled invoice is not counted cancelled",
			func(l string) string { return strings.Replace(l, "cancelled=1", "cancelled=0", 1) }},
	}
	suites := map[string]Suite{}
	for _, slip := range slips {
		s, ok := suites[slip.suite]
		if !ok {
			var err error
			if s, err = Load(packSuite(t, root, slip.suite)); err != nil {
				t.Fatal(err)
			}
			suites[slip.suite] = s
		}
		c, ok := s.Case(slip.caseID)
		if !ok {
			t.Fatalf("no case %s", slip.caseID)
		}
		var steps []OracleStep
		changed := false
		for _, st := range c.Oracle {
			if st.Kind != StepRun {
				steps = append(steps, st)
				continue
			}
			mutated := slip.mutate(st.Line)
			changed = changed || mutated != st.Line
			for _, line := range strings.Split(mutated, "\n") {
				steps = append(steps, OracleStep{Kind: StepRun, Line: line})
			}
		}
		if !changed {
			t.Fatalf("%s: the slip %q changed no step", slip.caseID, slip.why)
		}
		c.Oracle = steps
		ev, why := offlineOracle(t, py, s, c)
		if why != "" {
			t.Fatalf("%s: %s", slip.caseID, why)
		}
		if r := Grade(c, ev, Mode{Expert: true}); r.Passed {
			t.Errorf("%s: %s, and the graders passed it", slip.caseID, slip.why)
		}
	}
}

// checkOfficeSuite holds what every office suite promises: each case is an independent instance (no carried state, so
// a run from a taught worker's snapshot measures each case from that one teaching), case 1 is the ordinary one a person
// demonstrates, the committing controls are approved, and a case that files needs the OTP only the person has.
func checkOfficeSuite(t *testing.T, s Suite, commits []string) {
	t.Helper()
	if s.CarryState {
		t.Errorf("%s: each case starts from the snapshot; it must not carry state", s.Name)
	}
	if s.Cases[0].Category != "ordinary" {
		t.Errorf("%s: case 1 (the demonstrated one) is %s, not ordinary", s.Name, s.Cases[0].Category)
	}
	for _, c := range s.Cases {
		approved := map[string]bool{}
		for _, a := range c.Approves {
			approved[a.Control] = true
		}
		for _, want := range commits {
			if !approved[want] {
				t.Errorf("%s: %q is not approved", c.ID, want)
			}
		}
		files := false
		for _, g := range c.Expected.Graders {
			if g.Collection == "tn_returns" && g.Kind == "mock_records" {
				files = true
			}
		}
		if !files {
			continue
		}
		f, ok := c.Fact("otp")
		if !ok || !f.IsRequired() {
			t.Errorf("%s files a return without requiring the OTP", c.ID)
			continue
		}
		for name, b := range c.Files {
			if strings.Contains(string(b), strings.TrimSuffix(strings.TrimPrefix(f.Answer, "I called the client: the OTP is "), ".")) {
				t.Errorf("%s: the input %s holds the OTP", c.ID, name)
			}
		}
	}
}
