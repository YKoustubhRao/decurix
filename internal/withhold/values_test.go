package withhold

import (
	"strings"
	"testing"
)

// Values gives what withholding took out, never the words around it: a labelled password, the bare values of a
// reply to a question for a secret, and the whole of a text withheld whole.
func TestValuesAreWhatWasWithheld(t *testing.T) {
	cases := []struct {
		name, original, withheld string
		want                     []string
	}{
		{"a labelled value", "sign in as mira, password: Plum7Orbit", Text("sign in as mira, password: Plum7Orbit"), []string{"Plum7Orbit"}},
		{"a reply to a password question", "sure, the password is Fern42Kite ok",
			Reply("I need the password for mira.k. What is it?", "sure, the password is Fern42Kite ok"), []string{"Fern42Kite"}},
		{"a bare reply", "Fern42Kite", Reply("What is the password?", "Fern42Kite"), []string{"Fern42Kite"}},
		{"withheld whole", "  the vault  code ", Secret, []string{"the vault  code", "the vault code"}},
		{"nothing withheld", "file the bills", "file the bills", nil},
		{"not span by span", "abc", "xyz " + Marker, []string{"abc"}},
	}
	for _, c := range cases {
		got := Values(c.original, c.withheld)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: Values = %q, want %q (withheld %q)", c.name, got, c.want, c.withheld)
		}
		for _, v := range got {
			if strings.EqualFold(v, "password") {
				t.Errorf("%s: a label was taken for a value", c.name)
			}
		}
	}
}
