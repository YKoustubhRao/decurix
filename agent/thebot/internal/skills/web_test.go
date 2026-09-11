package skills

import "testing"

// extractQuery feeds the real search engines, so leftover instruction
// words directly degrade result quality. Found live: every single search
// went out with a stray leading "for" - "search the web for the newest Go
// version" was sent to DuckDuckGo as "for the newest Go version".
func TestExtractQueryStripsTheInstruction(t *testing.T) {
	cases := map[string]string{
		"search the web for the newest Go version": "the newest Go version",
		"search for RTX 5080 laptop prices":        "RTX 5080 laptop prices",
		"search the web for RTX 5080 laptops":      "RTX 5080 laptops",
		"google golang generics":                   "golang generics",
		"look up the latest nvidia news online":    "latest nvidia news online",
		"research quantum computing":               "quantum computing",
		"find out what the newest Go version is":   "what the newest Go version is",
		"please can you search online for go 1.27": "go 1.27",
		`search the web for "exact phrase"`:        "exact phrase",
	}
	for step, want := range cases {
		if got := extractQuery(step); got != want {
			t.Errorf("extractQuery(%q) = %q, want %q", step, got, want)
		}
	}
}

// A step with no instruction wrapper is already the query.
func TestExtractQueryLeavesABareSubjectAlone(t *testing.T) {
	if got := extractQuery("newest Go version"); got != "newest Go version" {
		t.Errorf("extractQuery() = %q, want the subject unchanged", got)
	}
}

// Stripping must never leave an empty query - that would search for
// nothing at all.
func TestExtractQueryNeverReturnsEmpty(t *testing.T) {
	for _, step := range []string{"search the web for", "google", "search online"} {
		if got := extractQuery(step); got == "" {
			t.Errorf("extractQuery(%q) = empty, want a fallback to the original step", step)
		}
	}
}
