package skills

import "testing"

// GST suite T10 (2026-10-22): an approved chain ran whole again, unticked the declaration it had ticked, and the
// approved filing click was refused - three approvals for one filing.
func TestPausedRestKeepsOnlyTheStoppedActionAndWhatFollows(t *testing.T) {
	reason := `on screen it would click button "FILE WITH EVC" (e9174) in "File GSTR-1 | Goods and Services Tax - Chromium": "FILE WITH EVC" sends, pays, buys or publishes something on your behalf`
	cases := map[string]string{
		`gui click e9171 && gui select e9172 "Rohan Dsouza" && gui click e9174`: `gui click e9174`,
		`gui type e9304 "22881.36" && gui click e9174 --checked && gui observe`: `gui click e9174 --checked && gui observe`,
		`gui select e9172 "A && B" && gui click e9174`:                          `gui click e9174`,
		`gui click e9174`:                                                       `gui click e9174`,
		`gui click e9174 && gui observe`:                                        `gui click e9174 && gui observe`,
		`gui click e9171; gui click e9174`:                                      `gui click e9171; gui click e9174`,
		`gui click e9171 && gui click --see "the file button"`:                  `gui click e9171 && gui click --see "the file button"`,
		`gui click e9174 && gui click e9171 && gui click e9174`:                 `gui click e9174 && gui click e9171 && gui click e9174`,
		`cd /tmp && gui click e9174`:                                            `cd /tmp && gui click e9174`,
		`gui click e9171 && gui type e9179 "$(cat otp.txt)" && gui click e9174`: `gui click e9174`,
		`gui click e9171 && gui type e9179 $(cat otp.txt) && gui click e9174`:   `gui click e9171 && gui type e9179 $(cat otp.txt) && gui click e9174`,
	}
	for command, want := range cases {
		if got := pausedRest(command, reason); got != want {
			t.Errorf("pausedRest(%q)\n got %q\nwant %q", command, got, want)
		}
	}
	if got := pausedRest(`gui click e1 && gui click e2`, "on screen it would press Return"); got != `gui click e1 && gui click e2` {
		t.Errorf("no ref named: the whole command, got %q", got)
	}
}
