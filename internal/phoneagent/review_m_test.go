package phoneagent

import "testing"

// A saved sign-in is offered only to the control plane it was made for
// (review 2026-11-03, B-18).
func TestASavedTokenGoesOnlyToItsOwnControlPlane(t *testing.T) {
	saved := Saved{Base: "https://a.example.com", Token: "t", Name: "Pixel"}
	for base, want := range map[string]bool{
		"https://a.example.com":  true,
		"https://A.example.com/": true,
		"https://b.example.com":  false,
		"http://a.example.com":   false,
	} {
		if got := SameControlPlane(saved, base); got != want {
			t.Errorf("SameControlPlane(%q) = %v, want %v", base, got, want)
		}
	}
	if SameControlPlane(Saved{Base: "https://a.example.com"}, "https://a.example.com") {
		t.Error("an empty token counts as a pairing")
	}
}
