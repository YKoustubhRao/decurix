package withhold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Go holds the same values for a marked message as the agent does: every value testdata/marked-values.json gives
// (computed by agent privacy.marked_values; agent/tests read the same file), and nothing more than the message
// whole in its other spellings.
func TestMarkedValuesFollowTheSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "marked-values.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Text   string   `json:"text"`
			Values []string `json:"values"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 5 {
		t.Fatalf("%d cases", len(f.Cases))
	}
	for _, c := range f.Cases {
		got := map[string]bool{}
		for _, v := range MarkedValues(c.Text) {
			got[v] = true
		}
		want := map[string]bool{}
		for _, v := range c.Values {
			want[v] = true
			if !got[v] {
				t.Errorf("%q: Go does not hold %q (the agent does)", c.Text, v)
			}
		}
		whole := map[string]bool{strings.TrimSpace(c.Text): true, strings.Join(strings.Fields(c.Text), " "): true}
		for v := range got {
			if !want[v] && !whole[v] {
				t.Errorf("%q: Go holds %q, which the agent does not", c.Text, v)
			}
		}
	}
}

// The live case's input class: a marked request holding an amount, a date and an unlabelled password-shaped token.
// The token is held; the amount and the date are its data and are not.
func TestAMarkedRequestHoldsItsUnlabelledTokenNotItsData(t *testing.T) {
	text := "Pay the March invoice of Rs. 1,250.00 due 03/04/2026 and sign in with Fern42Kite first."
	held := map[string]bool{}
	for _, v := range MarkedValues(text) {
		held[v] = true
	}
	if !held["Fern42Kite"] {
		t.Fatalf("the unlabelled token is not held: %q", MarkedValues(text))
	}
	for _, data := range []string{"1,250.00", "03/04/2026", "Rs.", "March"} {
		if held[data] {
			t.Errorf("data %q is held", data)
		}
	}
}
