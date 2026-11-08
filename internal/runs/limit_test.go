package runs

import "testing"

// The bundle limit setting: empty is the default, a number in bounds is taken, one outside is held to the nearest
// bound and said, and anything else is the default, said.
func TestBundleLimitSetting(t *testing.T) {
	cases := []struct {
		raw   string
		want  int64
		noted bool
	}{
		{"", DefaultLimit, false},
		{"  ", DefaultLimit, false},
		{"64", 64 << 20, false},
		{"8", 8 << 20, false},
		{"1024", 1024 << 20, false},
		{" 200 ", 200 << 20, false},
		{"7", 8 << 20, true},
		{"0", 8 << 20, true},
		{"-5", 8 << 20, true},
		{"1025", 1024 << 20, true},
		{"99999999999999999999", DefaultLimit, true},
		{"64MB", DefaultLimit, true},
		{"1.5", DefaultLimit, true},
	}
	for _, c := range cases {
		got, note := BundleLimit(c.raw)
		if got != c.want || (note != "") != c.noted {
			t.Errorf("BundleLimit(%q) = %d, %q; want %d, noted %v", c.raw, got, note, c.want, c.noted)
		}
	}
}
