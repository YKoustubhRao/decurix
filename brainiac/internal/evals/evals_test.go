package evals

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChecks(t *testing.T) {
	cases := []struct {
		check  Check
		answer string
		want   bool
	}{
		{Check{Kind: "number", Value: "12"}, "First 3*4 = 12, so the answer is 12.", true},
		{Check{Kind: "number", Value: "12345"}, "<think>maybe 9</think>Total: \\boxed{12,345}", true},
		{Check{Kind: "number", Value: "0.5"}, "The probability is 0.5000001", true},
		{Check{Kind: "number", Value: "12"}, "It is 12 or maybe 13", false},
		{Check{Kind: "exact", Value: "Paris"}, "Paris.", true},
		{Check{Kind: "exact", Value: "Paris"}, "The answer is Paris", true},
		{Check{Kind: "contains", Values: []string{"JOIN", "IS NULL"}}, "SELECT c.* FROM c LEFT JOIN o ON ... WHERE o.id is null", true},
		{Check{Kind: "contains", Values: []string{"section 138"}}, "It falls under Section 138 of the NI Act", true},
		{Check{Kind: "regex", Value: `(?i)\bnot\s+binding\b`}, "Such a clause is not binding.", true},
		{Check{Kind: "choice", Value: "C"}, "Looking at each option... The answer is (C).", true},
		{Check{Kind: "choice", Value: "C"}, "B", false},
		{Check{Kind: "choice", Value: "B"}, "Reasoning...\nB", true},
	}
	for _, c := range cases {
		if got := c.check.Correct(c.answer); got != c.want {
			t.Errorf("%+v on %q = %v, want %v", c.check, c.answer, got, c.want)
		}
	}
}

func TestCompareNeedsEnoughCases(t *testing.T) {
	r := &Results{}
	r.Set("math-7b", "math", Score{N: 5, Correct: 5})
	r.Set("remote", "math", Score{N: 5, Correct: 3})
	if _, known := r.Compare("math-7b", "remote", "math"); known {
		t.Fatal("five cases decided a comparison")
	}
	r.Set("math-7b", "math", Score{N: 40, Correct: 34})
	r.Set("remote", "math", Score{N: 40, Correct: 33})
	if better, known := r.Compare("math-7b", "remote", "math"); !known || !better {
		t.Fatalf("better %v known %v", better, known)
	}
	if better, _ := r.Compare("remote", "math-7b", "math"); better {
		t.Fatal("the worse model compared as better")
	}
}

func TestLowerBoundReflectsHowLittleASmallSampleSays(t *testing.T) {
	small, large := Score{N: 20, Correct: 20}.LowerBound(), Score{N: 200, Correct: 200}.LowerBound()
	if small > 0.90 || large < 0.98 || small >= large {
		t.Fatalf("20/20 claims %.3f, 200/200 claims %.3f", small, large)
	}
	if got := (Score{N: 100, Correct: 50}).LowerBound(); got < 0.40 || got > 0.45 {
		t.Fatalf("50/100 claims %.3f", got)
	}
	if got := (Score{}).LowerBound(); got != 0 {
		t.Fatalf("no cases claims %.3f", got)
	}
}

func TestProvenNeedsMoreThanAnEqualSmallScore(t *testing.T) {
	r := &Results{}
	set := func(a, b Score) {
		r.Set("cheap", "math", a)
		r.Set("remote", "math", b)
	}
	cases := []struct {
		name string
		a, b Score
		want bool
	}{
		{"too few cases", Score{N: 10, Correct: 10}, Score{N: 10, Correct: 5}, false},
		{"20/20 against a perfect score could be chance", Score{N: 20, Correct: 20}, Score{N: 20, Correct: 20}, false},
		{"20/20 against 80% is clear", Score{N: 20, Correct: 20}, Score{N: 20, Correct: 16}, true},
		{"60/60 against a perfect score", Score{N: 60, Correct: 60}, Score{N: 60, Correct: 60}, true},
		{"one point ahead on 40 is not enough", Score{N: 40, Correct: 36}, Score{N: 40, Correct: 35}, false},
		{"worse on many cases", Score{N: 200, Correct: 160}, Score{N: 200, Correct: 190}, false},
		{"close on many cases", Score{N: 400, Correct: 376}, Score{N: 400, Correct: 380}, true},
	}
	for _, c := range cases {
		set(c.a, c.b)
		if got := r.Proven("cheap", "remote", "math"); got != c.want {
			t.Errorf("%s: %v (claims %.3f)", c.name, got, c.a.LowerBound())
		}
	}
	if (*Results)(nil).Proven("a", "b", "c") {
		t.Error("no results proved something")
	}
}

func TestOutcomesPersistAndOnlyAMissMoves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.json")
	o := OpenOutcomes(p)
	t1 := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	if err := o.Record("m", "math", true, t1); err != nil {
		t.Fatal(err)
	}
	if got := o.Get("m", "math"); got.Passed != 1 || !got.LastMiss.IsZero() {
		t.Fatalf("%+v", got)
	}
	o.Record("m", "math", false, t1)
	o.Record("m", "math", false, t1.Add(-time.Hour)) // an older report does not move it back
	again := OpenOutcomes(p)
	if got := again.Get("m", "math"); got.Missed != 2 || !got.LastMiss.Equal(t1) {
		t.Fatalf("%+v", got)
	}
	os.WriteFile(p, []byte("not json"), 0o644)
	if got := OpenOutcomes(p).Get("m", "math"); got.Missed != 0 {
		t.Fatalf("a broken file: %+v", got)
	}
}

func TestReadCasesChecksShape(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	os.WriteFile(good, []byte(`{"id":"a","domain":"math","prompt":"1+1?","check":{"kind":"number","value":"2"}}`+"\n// a comment\n\n"), 0o644)
	cs, err := ReadCases(good)
	if err != nil || len(cs) != 1 {
		t.Fatalf("cases %v err %v", cs, err)
	}
	bad := filepath.Join(dir, "bad.jsonl")
	os.WriteFile(bad, []byte(`{"domain":"math","prompt":"x","check":{"kind":"vibes"}}`), 0o644)
	if _, err := ReadCases(bad); err == nil {
		t.Fatal("an unknown check kind was accepted")
	}
	r, _ := Load(filepath.Join(dir, "none.json"))
	r.Set("m", "math", Score{N: 1, Correct: 1})
	p := filepath.Join(dir, "evals.json")
	if err := r.Save(p); err != nil {
		t.Fatal(err)
	}
	r2, err := Load(p)
	if err != nil || r2.Scores["m"]["math"].Correct != 1 {
		t.Fatalf("round trip %v %v", r2, err)
	}
}
