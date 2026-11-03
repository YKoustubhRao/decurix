package phoneagent

import (
	"strings"
	"testing"
)

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

// A password field's text never reaches the control plane, even when Android
// shows it in the dump (review 2026-11-03, B-22).
func TestAPasswordFieldsTextIsNotSent(t *testing.T) {
	dump := strings.Replace(sampleDump, `text="" resource-id="" class="android.widget.EditText" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="true" password="true"`,
		`text="4711" resource-id="" class="android.widget.EditText" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="true" password="true"`, 1)
	if dump == sampleDump {
		t.Fatal("the sample's password field was not found")
	}
	d, err := parseDump([]byte(dump))
	if err != nil {
		t.Fatal(err)
	}
	tree := treeFor(d, 12, "com.whatsapp", 720, 1520)
	found := false
	for _, n := range tree.Nodes {
		if n.Password {
			found = true
			if n.Value != "" {
				t.Fatalf("a password field's text was sent: %+v", n)
			}
		}
	}
	if !found {
		t.Fatal("no password field in the tree")
	}
}
