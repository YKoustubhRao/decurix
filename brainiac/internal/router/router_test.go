package router

import (
	"testing"

	"brainiac/internal/catalog"
)

func trained(t *testing.T) *Router {
	t.Helper()
	cat, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	r := New()
	for _, d := range cat.Domains() {
		r.Add(d.ID, d.Description)
		for _, ex := range d.Examples {
			r.Add(d.ID, ex)
		}
	}
	for _, ex := range cat.GeneralExamples {
		r.Add(General, ex)
	}
	return r
}

// These are not in the catalog's examples: the router must generalise.
var heldOut = map[string]string{
	"What is the sum of the interior angles of a hexagon?":                            "math",
	"Solve 5x + 7 = 2x - 8":                                                           "math",
	"Calculate the compound interest on 1 lakh at 8% for 3 years":                     "finance",
	"Is GST charged on export of services?":                                           "finance",
	"Write a Python script to download all images from a web page":                    "code",
	"Why is my for loop in Java throwing ArrayIndexOutOfBoundsException?":             "code",
	"SELECT * FROM orders o LEFT JOIN customers c ON o.cid = c.id WHERE c.id IS NULL": "sql",
	"Can a minor sign a valid contract under the Indian Contract Act?":                "law",
	"What is the punishment for cheating under section 420 IPC?":                      "law",
	"What are the symptoms of typhoid and how is it treated?":                         "medicine",
	"Is it safe to take cetirizine every day?":                                        "medicine",
	"What is Avogadro's number and why does it matter?":                               "science",
	"Translate to Hindi: The invoice is attached":                                     "translate",
	"मेरा पैसा कब वापस आएगा? इसे अंग्रेज़ी में बताइए":                                 "translate",
	"hey there":                         General,
	"write a thank you note to my team": General,
	"what's a good name for a bakery?":  General,
}

func TestRouterGeneralisesToUnseenRequests(t *testing.T) {
	r := trained(t)
	misrouted, routed, fields := 0, 0, 0
	for text, want := range heldOut {
		d := r.Classify(text)
		if want != General {
			fields++
		}
		switch {
		case d.Routes() && d.Class != want:
			misrouted++
			t.Logf("WRONG %-10s sent to %-10s %.2f  %q", want, d.Class, d.P, text)
		case d.Routes():
			routed++
			t.Logf("route %-10s %.2f  %q", want, d.P, text)
		default:
			t.Logf("brain %-10s (best %s %.2f)  %q", want, d.Class, d.P, text)
		}
	}
	// The bar: nothing goes to the wrong specialist; most field requests reach theirs.
	if misrouted > 0 {
		t.Fatalf("%d held-out requests sent to the wrong field", misrouted)
	}
	if routed*3 < fields*2 {
		t.Fatalf("only %d of %d field requests reached their specialist", routed, fields)
	}
}

func TestShapesCarryAcrossWords(t *testing.T) {
	for _, tc := range []struct{ text, tok string }{
		{`\frac{1}{2} + \sqrt{x}`, "§latex"}, {"x = 3y + 2", "§equation"}, {"```go\nfunc main() {}\n```", "§code"},
		{"select a from b group by a", "§sql"}, {"₹ 5,000 at 18%", "§money"}, {"under section 138", "§provision"},
		{"मुझे कल बैंक जाना है", "§script:devanagari"},
	} {
		found := false
		for _, tok := range Tokens(tc.text) {
			if tok == tc.tok {
				found = true
			}
		}
		if !found {
			t.Errorf("%q has no %s: %v", tc.text, tc.tok, Tokens(tc.text))
		}
	}
}

func TestEmptyRouterSaysGeneral(t *testing.T) {
	if d := New().Classify("integrate x^2"); d.Class != General {
		t.Fatalf("got %+v", d)
	}
}
