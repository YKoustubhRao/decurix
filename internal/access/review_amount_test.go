package access

import "testing"

// An approval threshold errs high: a small marked figure never hides a large one, and with no money at all the
// amount is unknown, so the policy applies.
func TestLargestAmountErrsHigh(t *testing.T) {
	cases := map[string]float64{
		"Pay 450000 to Acme, bank charges Rs 50": 450000,
		"Pay 3 Cr to Acme, fee Rs 500":           3e7,
		"Pay Rs 4,50,000 incl GST 18%":           450000,
		"Transfer ₹1,200":                        1200,
	}
	for text, want := range cases {
		got := LargestAmount(text)
		if got == nil || *got != want {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
	for _, text := range []string{"Pay 3 invoices", "Pay the vendor", "delete 3 files"} {
		if got := LargestAmount(text); got != nil {
			t.Errorf("%q: got %v, want unknown (the policy applies)", text, *got)
		}
	}
}
