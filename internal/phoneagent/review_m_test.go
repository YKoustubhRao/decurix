package phoneagent

import (
	"context"
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

// Setting up ADB never opens adb's legacy network port (review 2026-11-03,
// B-26): it stays on Wireless debugging's own paired connection.
func TestConnectingADBNeverOpensTheLegacyPort(t *testing.T) {
	var calls [][]string
	defer func(r func(context.Context, ...string) ([]byte, error), p func(context.Context, string) (string, error)) {
		runADB, pickSerial = r, p
	}(runADB, pickSerial)
	runADB = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("connected"), nil
	}
	pickSerial = func(_ context.Context, want string) (string, error) { return want, nil }

	serial, err := connectWireless(context.Background(), "localhost:41234")
	if err != nil || serial != "localhost:41234" {
		t.Fatalf("serial %q, %v", serial, err)
	}
	for _, c := range calls {
		if strings.Join(c, " ") != "connect localhost:41234" {
			t.Fatalf("adb was asked to %v", c)
		}
	}
}
