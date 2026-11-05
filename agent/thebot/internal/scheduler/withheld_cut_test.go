package scheduler

import (
	"context"
	"strings"
	"testing"

	"thebot/internal/atomizer"
	"thebot/internal/skills"
	"thebot/internal/withheld"
)

// The value straddles every cut the scheduler makes (70, 80, 200, 400 bytes), and a wholly secret text runs over
// several lines: withheld first, then shortened, none of it leaves (review B2).
const cutSecret = "plumtree-cabinet-41"

func straddling(at int) string {
	return strings.Repeat("a", at-5) + " " + cutSecret + " and the rest of it"
}

func noPart(t *testing.T, where, got string) {
	t.Helper()
	if strings.Contains(got, "plum") || strings.Contains(got, "cabinet") {
		t.Fatalf("%s leaves part of the value: %q", where, got)
	}
}

func TestEveryCutWithholdsFirst(t *testing.T) {
	secrets := withheld.Clean([]string{cutSecret})
	ctx := withheld.With(context.Background(), []string{cutSecret})
	e := &execution{ctx: ctx}

	noPart(t, "a step line", e.cut(straddling(80), 80))
	noPart(t, "the resume line", e.cut(straddling(70), 70))

	noPart(t, "a request approval", approvalQuestion(&pause{action: requestAction, reason: "it pays"}, straddling(400), secrets))
	noPart(t, "a command approval", approvalQuestion(&pause{action: "pay --key " + cutSecret, reason: "it pays"}, "", secrets))
	shown := approvalQuestion(&pause{action: "raw " + cutSecret, shown: "gui type e5 '[withheld]'", reason: "r"}, "", secrets)
	if !strings.Contains(shown, "gui type e5") {
		t.Fatalf("the skill's shown form is not quoted: %q", shown)
	}
	noPart(t, "a shown action", shown)

	whole := "sign in to the portal as mira.k\nwith the password " + cutSecret + "\nand fetch the statement for March"
	wholeSet := withheld.Clean([]string{whole})
	if q := approvalQuestion(&pause{action: requestAction, reason: "r"}, whole, wholeSet); strings.Contains(q, "mira.k") ||
		!strings.Contains(q, withheld.Mark) {
		t.Fatalf("a wholly secret request is quoted: %q", q)
	}

	n := &node{step: atomizer.Step{ID: 1, Text: straddling(200)}, state: StepDone,
		result: skills.Result{Output: straddling(1200)}}
	noPart(t, "the plain summary", deterministicSummary([]*node{n}, secrets))
	noPart(t, "a one-step answer", singleAnswer(skills.Result{SkillName: "terminal", Output: "\n\nStdout:\n" + straddling(6000),
		Meta: map[string]any{"command": "cat notes"}}, secrets))
	noPart(t, "a paused step's question", pauseFor(&node{result: skills.Result{Output: straddling(1500), Meta: map[string]any{}}},
		secrets).question)
}

type namedSkill struct{}

func (namedSkill) Name() string                { return "stub" }
func (namedSkill) Description() string         { return "" }
func (namedSkill) Matches(skills.Subtask) bool { return true }
func (namedSkill) Run(skills.RunContext, skills.Subtask) (skills.Result, error) {
	return skills.Result{Output: "ok"}, nil
}

func TestTheSkillLineWithholdsFirst(t *testing.T) {
	var lines []string
	rc := skills.RunContext{Ctx: withheld.With(context.Background(), []string{cutSecret})}
	(&Scheduler{}).runOnce(namedSkill{}, rc, skills.Subtask{Step: straddling(70), Type: "general"},
		func(l string) { lines = append(lines, l) })
	noPart(t, "the skill line", strings.Join(lines, "\n"))
}
