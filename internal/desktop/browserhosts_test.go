package desktop

// Tests of mapping reserved test host names in a normal desktop's browser (browserhosts.go): which names may be
// mapped, and that a mapping replaces the last one and is removed whole.

import (
	"strings"
	"testing"
)

func TestOnlyReservedTestNamesMayBeMapped(t *testing.T) {
	for _, h := range []string{"app.example.test", "TaxNet.Example.Test", "portal.example", "x.invalid", "mock.localhost"} {
		if !ReservedTestHost(h) {
			t.Errorf("%q refused", h)
		}
	}
	for _, h := range []string{"bank.example.com", "app.com", "example.com", "test", "localhost", "a.test.com", "app.test:8000",
		"a b.test", "", ".test", "app.example.test.evil.com"} {
		if ReservedTestHost(h) {
			t.Errorf("%q may be mapped", h)
		}
	}
	if _, err := browserHostsBlock([]string{"app.example.test", "bank.example.com"}, "127.0.0.1:18080"); err == nil {
		t.Fatal("a real name was mapped beside a test one")
	}
	for _, target := range []string{"practice-mock:8000", "10.0.0.5:80", "127.0.0.1", "127.0.0.1:80", "127.0.0.1:99999"} {
		if _, err := browserHostsBlock([]string{"app.example.test"}, target); err == nil {
			t.Errorf("target %q taken", target)
		}
	}
}

func TestAMappingReplacesTheLastAndIsRemovedWhole(t *testing.T) {
	base := []byte("CHROMIUM_FLAGS=\"--x\"\nulimit -c 0 2>/dev/null\n")
	one, err := browserHostsBlock([]string{"a.example.test"}, "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(one, "set -- '--host-resolver-rules=MAP a.example.test 127.0.0.1:18080' \"$@\"") {
		t.Fatalf("block:\n%s", one)
	}
	two, _ := browserHostsBlock([]string{"b.example.test", "C.Example.Test"}, "127.0.0.1:18081")
	got := withBrowserHosts(withBrowserHosts(base, one), two)
	if strings.Count(string(got), browserHostsStart) != 1 || strings.Contains(string(got), "a.example.test") ||
		!strings.Contains(string(got), "MAP b.example.test 127.0.0.1:18081,MAP c.example.test 127.0.0.1:18081") {
		t.Fatalf("second mapping did not replace the first:\n%s", got)
	}
	if !strings.HasPrefix(string(got), string(base)) {
		t.Fatalf("the image's own flags changed:\n%s", got)
	}
	if back := withBrowserHosts(got, ""); string(back) != string(base) {
		t.Fatalf("unmapped:\n%q\nwant\n%q", back, base)
	}
	// A file without a trailing new line keeps its last line whole.
	if got := withBrowserHosts([]byte("A=1"), one); !strings.HasPrefix(string(got), "A=1\n"+browserHostsStart) {
		t.Fatalf("%q", got)
	}
	// Unmapping a file with no block changes nothing.
	if got := withBrowserHosts(base, ""); string(got) != string(base) {
		t.Fatalf("%q", got)
	}
}
