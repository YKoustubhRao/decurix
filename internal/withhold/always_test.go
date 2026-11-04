package withhold

import (
	"strings"
	"testing"
)

// A value held for its owner's life is withheld wherever it is, by Known and by Text, until its owner lets it go; a
// short one is never held (review RES-CP-1, B7 interim).
func TestAValueHeldForItsOwnersLifeIsWithheldEverywhere(t *testing.T) {
	HoldAlways("screen:t1", "Qx7-pL2_")
	t.Cleanup(func() { ReleaseAlways("screen:t1") })
	for _, text := range []string{"VNC_PW=Qx7-pL2_\nHOME=/home/headless", "it is Qx7-pL2_.", "zzQx7-pL2_zz"} {
		if got := Known(text); strings.Contains(got, "Qx7-pL2_") || !strings.Contains(got, Marker) {
			t.Errorf("Known(%q) = %q", text, got)
		}
		if got := Text(text); strings.Contains(got, "Qx7-pL2_") {
			t.Errorf("Text(%q) = %q", text, got)
		}
	}
	if got := AlwaysFor("screen:t1"); len(got) != 1 || got[0] != "Qx7-pL2_" {
		t.Fatalf("AlwaysFor = %v", got)
	}
	if AlwaysFor("screen:other") != nil {
		t.Fatal("another owner holds a value")
	}

	HoldAlways("screen:old", "admin") // an old desktop's shared default: no secret, and it would cut words
	t.Cleanup(func() { ReleaseAlways("screen:old") })
	if got := Known("open the admin page"); got != "open the admin page" || AlwaysFor("screen:old") != nil {
		t.Fatalf("a short value was held: %q", got)
	}

	ReleaseAlways("screen:t1")
	if got := Known("Qx7-pL2_"); got != "Qx7-pL2_" {
		t.Fatalf("a released value is still withheld: %q", got)
	}
}
