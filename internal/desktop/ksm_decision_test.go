package desktop

import "testing"

// KSM is turned off only when Decurix turned it on, or the person asked; KSM an owner enabled for other work is left on
// and said so (REVIEW-3 follow-up 4).
func TestKSMIsTurnedOffOnlyWhenDecurixTurnedItOn(t *testing.T) {
	cases := []struct {
		explicit, on, marked bool
		want                 int
	}{
		{false, false, false, ksmLeave},
		{false, true, true, ksmTurnOff},
		{true, true, false, ksmTurnOff},
		{false, true, false, ksmSayOn},
	}
	for _, c := range cases {
		if got := ksmOffDecision(c.explicit, c.on, c.marked); got != c.want {
			t.Errorf("ksmOffDecision(%v, %v, %v) = %d, want %d", c.explicit, c.on, c.marked, got, c.want)
		}
	}
}
