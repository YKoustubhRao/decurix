package skills

import (
	"strings"
	"testing"
)

// Round 5 P4 (2026-10-20): after looking at ID pictures, a joiner was onboarded with phone 9999999999; the attached
// tracker had "+91 90040 55318". A filler is refused whatever was read; the real number, in any format, is not.
func TestAFillerIsRefusedEvenAfterPictures(t *testing.T) {
	st := groundingState("Onboard the joiners in the portal.")
	st.noteSeen("Name | Mobile\nSneha Iyer | +91 90040 55318")
	st.ShownPages = true
	cmd := `gui type e1956 "Sneha" && gui type e1960 "9999999999" && gui click e1961`
	nudge, values := groundingCheck(st, cmd)
	if !strings.Contains(nudge, `"9999999999" looks invented`) || !strings.Contains(nudge, "in the format the field accepts") {
		t.Fatalf("nudge = %q", nudge)
	}
	if q := untracedQuestion(values, nudge); !strings.Contains(q, "9999999999") {
		t.Errorf("question = %q", q)
	}
	for _, ok := range []string{`gui type e1960 "9004055318"`, `gui type e1960 "+91 90040 55318"`} {
		if n := groundingNudge(st, ok); n != "" {
			t.Errorf("%s was refused: %q", ok, n)
		}
	}
	for _, v := range []string{"0000000000", "1234567890", "test", "N/A", "sneha.iyer@example.com", "xxx"} {
		if !looksFiller(v) {
			t.Errorf("%q not taken for a filler", v)
		}
	}
	for _, v := range []string{"9004055318", "+91 90040 55318", "720000", "16/11/2026", "Sneha", "INV-11111"} {
		if looksFiller(v) {
			t.Errorf("%q taken for a filler", v)
		}
	}
	// A filler that IS in a source is that source's value.
	st.noteSeen("Bank account: 1111111111")
	if n := groundingNudge(st, `gui type e7 "1111111111"`); n != "" {
		t.Errorf("a value the source gives was refused: %q", n)
	}
}

func TestTheContractSaysNeverAFiller(t *testing.T) {
	if !strings.Contains(taskRules, "never a filler like 9999999999") {
		t.Error("taskRules lacks the line")
	}
}
