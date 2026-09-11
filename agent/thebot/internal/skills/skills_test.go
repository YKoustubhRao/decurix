package skills

import "testing"

// unmatchedType is a task type deliberately never given its own
// registered skill in this test - simulates exactly the real bug: a
// classifier exists (atomizer) but nothing in the skill registry claims
// its type.
const unmatchedType = "__test_never_registered__"

func TestPickFallsBackWhenNothingMatches(t *testing.T) {
	fb := &alwaysFailSkill{name: "test-fallback"}
	SetFallback(fb)
	defer SetFallback(nil) // don't leak state into other tests in this package

	got := Pick(Subtask{Step: "anything", Type: unmatchedType})
	if got == nil {
		t.Fatal("Pick() = nil even with a fallback set - the exact bug this guards against: an unmatched task type must never resolve to nothing")
	}
	if got.Name() != "test-fallback" {
		t.Errorf("Pick() returned %q, want the fallback skill", got.Name())
	}
}

func TestPickPrefersRealMatchOverFallback(t *testing.T) {
	real := &alwaysFailSkill{name: "real-match"}
	fb := &alwaysFailSkill{name: "fallback"}
	registerTestFailFixture(real, "test-real-match-type")
	SetFallback(fb)
	defer SetFallback(nil)

	got := Pick(Subtask{Step: "anything", Type: "test-real-match-type"})
	if got.Name() != "real-match" {
		t.Errorf("Pick() = %q, want the real matching skill, not the fallback", got.Name())
	}
}

func TestPickReturnsNilOnlyWhenFallbackNeverSet(t *testing.T) {
	SetFallback(nil)
	got := Pick(Subtask{Step: "anything", Type: unmatchedType})
	if got != nil {
		t.Errorf("Pick() = %v, want nil when no fallback was ever configured", got)
	}
}

// alwaysFailSkill and registerTestFailFixture are shared minimal test
// doubles - kept here rather than duplicated across test files in this
// package.
type alwaysFailSkill struct {
	name  string
	ttype string
}

func (s *alwaysFailSkill) Name() string        { return s.name }
func (s *alwaysFailSkill) Description() string { return "test double" }
func (s *alwaysFailSkill) Matches(st Subtask) bool {
	return s.ttype != "" && st.Type == s.ttype
}
func (s *alwaysFailSkill) Run(rc RunContext, st Subtask) (Result, error) {
	return Result{Subtask: st, SkillName: s.name, Output: "test failure", Verified: false, Meta: map[string]any{}}, nil
}

func registerTestFailFixture(s *alwaysFailSkill, ttype string) {
	s.ttype = ttype
	Register(s)
}
