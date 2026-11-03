package phonehost

import (
	"strings"
	"testing"
)

// A phone's app list keeps only package names and plain one-line names
// (review 2026-11-03, B-22).
func TestCleanAppsKeepsPackageNamesAndPlainNames(t *testing.T) {
	got := CleanApps([]App{
		{Package: "com.example.app", Name: "Example\nExec=other"},
		{Package: "../etc", Name: "x"},
		{Package: "com.example.empty", Name: "\t"},
		{Package: "com.example.long", Name: strings.Repeat("a", 300)},
	})
	if len(got) != 3 {
		t.Fatalf("apps = %+v", got)
	}
	if strings.ContainsAny(got[0].Name, "\r\n") {
		t.Fatalf("a line break survived: %q", got[0].Name)
	}
	if got[1].Name != "com.example.empty" || len([]rune(got[2].Name)) != 100 {
		t.Fatalf("apps = %+v", got)
	}
	if ValidPackage("com example") || ValidPackage("single") || !ValidPackage("com.example.app_2") {
		t.Fatal("ValidPackage")
	}
}

// Only typing and navigation keys reach a phone; system keys are refused at
// the control plane (review 2026-11-03, B-05).
func TestOnlyOrdinaryKeysReachAPhone(t *testing.T) {
	for _, code := range []int{4, 7, 29, 61, 66, 67, 112, 122, 19} {
		if !OrdinaryKey(code) {
			t.Errorf("key %d refused", code)
		}
	}
	for _, code := range []int{3, 5, 6, 24, 26, 27, 187, 219, 223, 0, -1} {
		if OrdinaryKey(code) {
			t.Errorf("system key %d allowed", code)
		}
	}
	s := &Session{}
	if err := s.Input(Input{Kind: "key", Key: 26}); err == nil {
		t.Fatal("a system key was passed to the phone")
	}
}

// A key's meta state keeps only the typing modifiers: a system modifier (META, ALT) on an allowed letter never reaches
// the phone (RW1-B-06).
func TestAKeyCarriesOnlyTypingModifiers(t *testing.T) {
	const shift, ctrl, alt, meta = 0x1, 0x1000, 0x2, 0x10000
	if got := OrdinaryMeta(shift | ctrl | alt | meta); got != shift|ctrl {
		t.Fatalf("OrdinaryMeta = %#x, want shift|ctrl", got)
	}
}
