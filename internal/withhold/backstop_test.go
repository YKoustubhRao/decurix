package withhold

import (
	"strings"
	"testing"
)

// The backstop's added labels and the dash separator catch these shapes of a typed credential (review 2026-11-03,
// C-03). Over-withholding is harmless here: the desktop still gets the text as typed.
func TestTheBackstopWithholdsShortLabelsAndADash(t *testing.T) {
	for _, in := range []string{
		"user ACME01, pass: Sunshine2024",
		"pw: hunter2abc",
		"Password - Tiger",
		"creds: admin:Adm1n@2024",
		"key: AbC123dEf456gHi789",
	} {
		got := Text(in)
		if !strings.Contains(got, Marker) {
			t.Errorf("Text(%q) = %q, want a value withheld", in, got)
		}
	}
}

// What the backstop still keeps is ordinary prose: no label, or a label with nothing secret after it.
func TestTheBackstopKeepsOrdinaryWords(t *testing.T) {
	for _, in := range []string{
		"pass the file to accounts",
		"the key points are below",
		"the password is incorrect",
	} {
		if got := Text(in); got != in {
			t.Errorf("Text(%q) = %q, want it unchanged", in, got)
		}
	}
}
