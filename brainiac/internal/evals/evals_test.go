package evals

import (
	"os"
	"path/filepath"
	"testing"
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
