//go:build linux

package desktop

import "testing"

// TestKSMIsOnlyOnWhenAskedFor: merging pages across desktops that distrust each other is a side channel between them,
// so it is off unless the owner turns it on (L-01).
func TestKSMIsOnlyOnWhenAskedFor(t *testing.T) {
	for _, c := range []struct {
		enable, disable string
		want            bool
	}{
		{"", "", false}, {"1", "", true}, {"true", "", true}, {"0", "", false}, {"1", "1", false}, {"yes", "", false},
	} {
		if got := ksmWanted(c.enable, c.disable); got != c.want {
			t.Errorf("ksmWanted(%q, %q) = %v", c.enable, c.disable, got)
		}
	}
}
