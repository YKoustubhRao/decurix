package access

import (
	"strings"
	"testing"
)

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

// A figure glued to its currency or scale, and the grouping other countries use, are read in full (review
// 2026-11-03: "Pay Rs450000 ... bank charges Rs 50" read as 50 and skipped the two-approver rule).
func TestLargestAmountGluedAndGrouped(t *testing.T) {
	cases := []struct {
		text string
		want *float64
	}{
		// Glued currency, before or after the figure.
		{"Pay Rs450000 to Acme, bank charges Rs 50", f(450000)},
		{"Rs450000 to Acme, fee Rs 100", f(450000)},
		{"Pay INR450000 to Acme, fee Rs 50", f(450000)},
		{"Pay INR450000 plus Rs 100 fee", f(450000)},
		{"Pay 450000INR, fee Rs 50", f(450000)},
		{"Pay ₹4,50,000 to Acme, fee ₹50", f(450000)},
		{"Pay Rs.450000, fee Rs 50", f(450000)},
		{"Pay USD1500 and fee USD 5", f(1500)},
		{"Pay Rs 4,50,000/- to Acme, fee Rs 50/-", f(450000)},
		// Scales, spaced or glued, and a large unmarked figure beside a small marked one.
		{"Pay $1.5M to Acme, fee $20", f(1.5e6)},
		{"Pay 3 Cr to Acme, fee Rs 500", f(3e7)},
		{"Pay 3Cr to Acme, fee Rs 500", f(3e7)},
		{"Pay 10 L to Acme (fee ₹200)", f(1e6)},
		{"Pay ₹5L now", f(5e5)},
		{"Pay $50k to Acme", f(5e4)},
		{"Pay USD 2bn", f(2e9)},
		{"Pay 1.5 million USD", f(1.5e6)},
		{"Pay Rs 5L/- to Acme", f(5e5)},
		{"Pay 450000 incl 18% GST Rs 81000", f(450000)},
		{"Pay 4,50,000.00 incl GST 18", f(450000)},
		// European, apostrophe and spaced grouping.
		{"Transfer 450.000,00 EUR, fee EUR 5", f(450000)},
		{"Pay EUR 1.234.567", f(1234567)},
		{"Pay CHF 1'250'000", f(1250000)},
		{"Pay 1 250 000,00 EUR", f(1250000)},
		{"Pay Rs 1,23,456.78", f(123456.78)},
		{"Pay EUR 12,50", f(12.5)},
		// A figure read two ways takes the larger reading.
		{"Pay EUR 1.250", f(1250)},
		{"Pay $0.125 per unit", f(0.125)},
		// Currency codes that are also words count only in capitals; "pounds" is a weight as often as money.
		{"Pay TRY 300", f(300)},
		{"try 3 times to pay the vendor", nil},
		{"ship 5 pounds of tea", nil},
		{"Hrs450 logged", nil},
		// No money anywhere: unknown, so the policy applies.
		{"Pay 4500000 to Acme", nil},
		{"Pay 1.5M to Acme", nil},
	}
	for _, c := range cases {
		got := LargestAmount(c.text)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%q: got %v want %v", c.text, deref(got), deref(c.want))
		}
	}
}

// Dates, times, units, firm prefixes and identifiers beside an amount are not amounts, so a small payment that
// mentions them does not ask for a second approver; a marked figure next to them still counts.
// A figure set aside as an identifier but larger than every money figure leaves the amount unknown, so the approval
// policy applies (A-05): which figure is the money is not something the words settle, and the smaller one must never
// win - "Rs 50 fee; pay No. 4500012345" read as 50 let a large payment pass a lower bar.
func TestALargerIdentifierLeavesTheAmountUnknown(t *testing.T) {
	for _, text := range []string{
		"Rs 50 fee; pay No. 4500012345",
		"Pay Rs 500 to a/c 50100123456789",
		"Pay Rs 500, Ref: 778899123",
		"Pay Rs 500 against invoice no. 45000",
		"Pay Rs 500 against PO 4500012345",
	} {
		if got := LargestAmount(text); got != nil {
			t.Errorf("%q: got %v, want unknown (nil)", text, *got)
		}
	}
	// An identifier smaller than the money figure settles nothing either way: the money figure stands.
	if got := LargestAmount("Pay Rs 5,000 against invoice no. 45"); got == nil || *got != 5000 {
		t.Errorf("small identifier: got %v, want 5000", deref(got))
	}
}

func TestLargestAmountSkipsDatesAndIdentifiers(t *testing.T) {
	cases := []struct {
		text string
		want float64
	}{
		{"Pay Rs 500 to Acme due 30 Nov 2026", 500},
		{"Pay Rs 500 to Acme due Nov 30, 2026", 500},
		{"Pay Rs 500 for March 2026", 500},
		{"Pay Rs 500 due 2026-11-30", 500},
		{"Pay Rs 500 at 10:30", 500},
		{"Pay Rs 500 for 5kg of rice", 500},
		{"Pay Rs 45,000 M/s Sharma Traders", 45000},
		{"Pay bill 45000, fee Rs 50", 45000}, // "bill 45000" may be the amount: it counts
		// A marked or unmistakable amount beside a month or a label still counts.
		{"Pay Rs 50000 March rent", 50000},
		{"Pay 50000 March rent, fee Rs 50", 50000},
		{"Pay Rs 500 and 2,50,000 to a/c 123", 250000},
	}
	for _, c := range cases {
		got := LargestAmount(c.text)
		if got == nil || *got != c.want {
			t.Errorf("%q: got %v want %v", c.text, deref(got), c.want)
		}
	}
}

// An amount policy that applies only because no amount could be read says so to the approver: the card and the
// refusal name the reason, so a stricter rule firing is not a mystery (REVIEW-3 follow-up 6).
func TestAnUnknownAmountIsToldToTheApprover(t *testing.T) {
	s := newStore(t)
	_, _ = s.SavePolicy(Policy{ActionMatch: `\bpay\b`, AmountOver: f(1000), ApproverRoles: []string{"finance"}})
	clerk, _ := s.CreateUser("clerk", "password-1", []string{"clerk"})

	known, _ := s.Required("w1", nil, "Approval needed: pay $5,000 to the supplier.")
	if known.AmountUnknown {
		t.Fatalf("an amount that was read is not unknown: %+v", known)
	}
	unknown, _ := s.Required("w1", nil, "Approval needed: pay the supplier's invoice.")
	if len(unknown.Policies) != 1 || !unknown.AmountUnknown {
		t.Fatalf("the policy applies because the amount is unknown, and says so: %+v", unknown)
	}
	d, _ := s.Decide(clerk, "w1", nil, "t1", "Approval needed: pay the supplier's invoice.", "yes")
	if d.Allowed || !strings.Contains(d.Message, "amount could not be read") {
		t.Fatalf("the refusal must say the amount was unknown: %+v", d)
	}
	d, _ = s.Decide(clerk, "w1", nil, "t2", "Approval needed: pay $5,000 to the supplier.", "yes")
	if strings.Contains(d.Message, "amount could not be read") {
		t.Fatalf("a known amount must not be called unknown: %+v", d)
	}
}
