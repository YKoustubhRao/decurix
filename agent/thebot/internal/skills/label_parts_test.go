package skills

import (
	"strings"
	"testing"
)

// P2's portal (2026-10-20): the product option "MUG-12 - Ceramic mug 12oz" (code + " - " + description, as the
// portal's order form lists it) was refused although the price list showed both parts, and the model then asked the
// person whether to omit an item that exists.
func TestALabelMadeOfSeenPartsIsTraced(t *testing.T) {
	st := groundingState("Enter every approved order in the portal.", "office read /w/price_list_Oct2026.xlsx")
	st.noteSeen("SKU | Item | Rate\nMUG-12 | Ceramic mug 12oz | 4.20\nWB-500 | Water bottle 500ml | 6.50\nCustomer: Cafe Bloom, City: Pune")
	for _, cmd := range []string{`gui type e12 "MUG-12 - Ceramic mug 12oz"`, `gui select e12 "MUG-12 - Ceramic mug 12oz"`,
		`gui type e12 "MUG-12 – Ceramic mug 12oz"`, `gui type e9 "Cafe Bloom (Pune)"`, `gui type e9 "WB-500 / Water bottle 500ml"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused: %q", cmd, n)
		}
	}
	// Strict: every part traced whole, only separators between.
	for _, cmd := range []string{`gui type e12 "MUG-12 - Ceramic mug 16oz"`, `gui type e12 "MUG-13 - Ceramic mug 12oz"`,
		`gui type e12 "MUG-99 - Ceramic mug 12oz"`, `gui type e12 "MUG-12 extra Ceramic mug 12oz"`} {
		if n := groundingNudge(st, cmd); n == "" {
			t.Errorf("%s was let through", cmd)
		}
	}
}

func TestARefusalSaysItIsAboutTheTextNotTheItem(t *testing.T) {
	st := groundingState("Enter every approved order in the portal.")
	n := groundingNudge(st, `gui type e12 "MUG-99 - Mystery mug"`)
	for _, want := range []string{"says nothing about whether the item or record exists", "type the code alone",
		"gui select REF"} {
		if !strings.Contains(n, want) {
			t.Errorf("nudge lacks %q: %s", want, n)
		}
	}
}
