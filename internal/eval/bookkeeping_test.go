// Tests of the matrix runner's bookkeeping rules (docs/evals.md, "Bookkeeping"), failure classes, ablation arms, and
// that nothing the worker can read tells one arm from another.
package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/practice"
)

// ---- no model turn ----

func TestNoTurnNeedsTheProviderToHaveFailedEveryCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     string
		observed string
		u        practice.CaseUsage
		want     bool
	}{
		{"every call throttled, then an error", ArmModel, ObservedError, practice.CaseUsage{Calls: 3, Failed: 3}, true},
		{"never reached, then the time ran out", ArmModel, ObservedTimeout, practice.CaseUsage{Unreached: 2}, true},
		{"one call answered", ArmModel, ObservedError, practice.CaseUsage{Calls: 3, Failed: 2}, false},
		{"a worker that failed before asking the model", ArmModel, ObservedError, practice.CaseUsage{}, false},
		{"a taught replay that finished with no model", ArmModel, ObservedFinal, practice.CaseUsage{}, false},
		{"a replay that timed out with no model", ArmModel, ObservedTimeout, practice.CaseUsage{}, false},
		{"a question after failed calls", ArmModel, ObservedQuery, practice.CaseUsage{Calls: 1, Failed: 1}, false},
		{"a calibration arm", ArmOracle, ObservedError, practice.CaseUsage{Calls: 1, Failed: 1}, false},
	} {
		if got := noTurn(tc.kind, tc.observed, tc.u); got != tc.want {
			t.Errorf("%s: noTurn=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// The rerun rule reads the row alone: two arms with the same row get the same decision, whatever else differs.
func TestTheRerunRuleIsTheSameForEveryArm(t *testing.T) {
	noTurnRow := Row{Invalid: true, InvalidWhy: InvalidNoTurn, Observed: OutcomeError}
	for _, arm := range []string{"default", "fast+expert@0123456789ab", "claude:default-playbooks"} {
		r := noTurnRow
		r.Arm = arm
		if !rerun(r) {
			t.Fatalf("arm %s: a no-turn row is not rerun", arm)
		}
		r.InvalidWhy = InvalidModelChanged
		if rerun(r) {
			t.Fatalf("arm %s: a model-changed row is rerun", arm)
		}
		if rerun(Row{Arm: arm, Observed: OutcomeError}) {
			t.Fatalf("arm %s: a worker's error is rerun", arm)
		}
	}
}

// flaky asks the model once per request and reports an error when the provider gave no answer, else finishes.
func flaky(fs *fakeSandbox, caseID, _ string, _ int) {
	fs.think(0)
	if u := fs.broker.CaseUsage(); u.Calls-u.Failed == 0 {
		fs.callback("/api/querytext", map[string]any{"kind": "error", "reply": "The model could not be reached."})
		return
	}
	fs.save(caseID, "out.txt", "3\n")
	fs.final("Totalled.", true)
}

// A case run the provider spoiled is stored as invalid, run again in every arm alike, and scored on the rerun.
func TestANoTurnCaseIsRerunInEveryArmAlike(t *testing.T) {
	s, sb, hub, _, _ := newService(t, flaky)
	// The first call of each arm is throttled (worker calls 1 and 3): each arm's case run has one no-turn row.
	hub.throttled = map[int]bool{1: true, 3: true}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"},
		Arms: []Arm{{Name: "default", Kind: ArmModel}, {Name: "default+expert", Kind: ArmModel, Expert: true}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 4 || strings.Contains(run.Note, "invalid") {
		t.Fatalf("%d rows, note %q", len(rows), run.Note)
	}
	for _, arm := range []string{"default", "default+expert"} {
		got := rowsBy(rows, arm, "office")
		if len(got) != 2 || !got[0].Invalid || got[0].InvalidWhy != InvalidNoTurn || got[0].Attempt != 1 || got[0].ProviderErrors != 1 ||
			got[1].Invalid || !got[1].Passed || got[1].Attempt != 2 || got[0].Repeat != got[1].Repeat {
			t.Fatalf("arm %s rows: %+v", arm, got)
		}
	}
	tab := Summarise(run, rows)
	for _, arm := range []string{"default", "default+expert"} {
		if c := tab.Totals[arm]; c.N != 1 || c.Pass != 1 || c.NoTurn != 1 || c.Paired != 0 || c.ProviderErrors != 1 {
			t.Fatalf("arm %s total: %+v", arm, c)
		}
	}
	if tab.Unscored != 0 || strings.Contains(tab.Markdown(), "INVALID") {
		t.Fatalf("a rerun that scored made the table invalid:\n%s", tab.Markdown())
	}
	// Each rerun started from the snapshot, as the case did (each arm's first run is on its new desktop).
	if sb.resets != 2 {
		t.Fatalf("resets: %d, want one before each rerun", sb.resets)
	}
}

// A case run still without a turn after every rerun stays unscored, the run note and the table say so, and the same
// case run is left out of the other model arm too.
func TestACaseRunWithNoTurnAfterEveryRerunIsUnscoredAndPaired(t *testing.T) {
	s, _, hub, _, _ := newService(t, flaky)
	hub.throttled = map[int]bool{1: true, 2: true, 3: true} // the first arm's three runs
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"},
		Arms: []Arm{{Name: "default", Kind: ArmModel}, {Name: "default+expert", Kind: ArmModel, Expert: true}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if got := rowsBy(rows, "default", "office"); len(got) != 1+MaxCellReruns {
		t.Fatalf("default ran %d times, want %d", len(got), 1+MaxCellReruns)
	}
	if !strings.Contains(run.Note, "no model turn after") {
		t.Fatalf("note: %q", run.Note)
	}
	tab := Summarise(run, rows)
	if tab.Unscored != 1 || tab.Totals["default"].N != 0 || tab.Totals["default+expert"].N != 0 || tab.Totals["default+expert"].Paired != 1 {
		t.Fatalf("table: unscored %d, default %+v, other %+v", tab.Unscored, tab.Totals["default"], tab.Totals["default+expert"])
	}
	md := tab.Markdown()
	if !strings.Contains(md, "**INVALID: 1 case run(s) had no model turn") || strings.Contains(md, "a model changed") {
		t.Fatalf("table head:\n%s", md)
	}
}

// ---- errored runs and efficiency ----

func TestErroredRunsCountZeroButStayOutOfTokens(t *testing.T) {
	rows := []Row{
		{Observed: OutcomeComplete, Passed: true, TokensIn: 100, TokensOut: 0, Minutes: 1},
		{Observed: OutcomeComplete, Passed: true, TokensIn: 200, TokensOut: 0, Minutes: 2},
		{Observed: OutcomeTimeout, TokensIn: 9000000, Minutes: 60},
		{Observed: OutcomeBudget, TokensIn: 5000000, Minutes: 30},
		{Observed: OutcomeError, TokensIn: 7, Minutes: 0.1},
	}
	c := cell(rows)
	if c.N != 5 || c.Pass != 2 || c.Errored != 3 || c.EfficiencyN != 2 {
		t.Fatalf("cell: %+v", c)
	}
	if c.TokensMedian != 150 || c.TokensP90 != 200 || c.MinutesMedian != 1.5 {
		t.Fatalf("efficiency over the runs that did not error: %+v", c)
	}
	// Every run errored: pass 0/n, no efficiency figure.
	if c := cell(rows[2:]); c.N != 3 || c.Pass != 0 || c.EfficiencyN != 0 || c.TokensMedian != 0 {
		t.Fatalf("all errored: %+v", c)
	}
}

// Infrastructure in one model arm leaves the same case run out of the others; calibration arms are never paired.
func TestRunsAreNeverExcludedFromOneArmOnly(t *testing.T) {
	run := Run{ID: "e1", Repeats: 2, Cases: []string{"a"}, Arms: []Arm{{Name: "x", Kind: ArmModel}, {Name: "y", Kind: ArmModel},
		{Name: "oracle", Kind: ArmOracle}}}
	rows := []Row{
		{Arm: "x", CaseID: "a", Repeat: 1, Observed: OutcomeComplete, Passed: true},
		{Arm: "x", CaseID: "a", Repeat: 2, Observed: OutcomeComplete, Passed: true},
		{Arm: "y", CaseID: "a", Repeat: 1, Observed: OutcomeComplete, Passed: true},
		{Arm: "y", CaseID: "a", Repeat: 2, Infra: true},
		{Arm: "oracle", CaseID: "a", Repeat: 1, Observed: OutcomeComplete, Passed: true},
		{Arm: "oracle", CaseID: "a", Repeat: 2, Observed: OutcomeComplete, Passed: true},
	}
	tab := Summarise(run, rows)
	if x := tab.Totals["x"]; x.N != 1 || x.Paired != 1 {
		t.Fatalf("x: %+v", x)
	}
	if y := tab.Totals["y"]; y.N != 1 || y.Infra != 1 || y.Paired != 0 {
		t.Fatalf("y: %+v", y)
	}
	if o := tab.Totals["oracle"]; o.N != 2 || o.Paired != 0 {
		t.Fatalf("oracle: %+v", o)
	}
	// An arm that has not run the case run yet leaves nothing out of the others.
	if x := Summarise(run, rows[:3]).Totals["x"]; x.N != 2 || x.Paired != 0 {
		t.Fatalf("x before y ran repeat 2: %+v", x)
	}
}

// The table states the rules and compares tokens, never money.
func TestTheTableStatesItsBookkeepingAndNoMoney(t *testing.T) {
	run, rows := fixtureRun(5)
	md := Summarise(run, rows).Markdown()
	if !strings.Contains(md, "- Bookkeeping: ") || !strings.Contains(md, "never in money") {
		t.Fatalf("no bookkeeping line:\n%s", md)
	}
	for _, money := range []string{"$", "USD", "cost_usd", "dollar"} {
		if strings.Contains(md, money) {
			t.Fatalf("the table shows money (%q):\n%s", money, md)
		}
	}
}

// ---- failure classes ----

func TestTheTableReportsPerFailureClass(t *testing.T) {
	run, rows := fixtureRun(5)
	run.Classes = map[string]string{"one": "date-order", "two": "date-order"}
	tab := Summarise(run, rows)
	if len(tab.Classes) != 1 || tab.Classes[0].Class != "date-order" || tab.Classes[0].Cases != 2 {
		t.Fatalf("classes: %+v", tab.Classes)
	}
	if c := tab.Classes[0].Arms["default"]; c.N != 9 || c.Pass != 8 { // "one" 5/5, "two" 3/4 (one paired out)
		t.Fatalf("default in the class: %+v", c)
	}
	if md := tab.Markdown(); !strings.Contains(md, "## By failure class") || !strings.Contains(md, "| date-order (2 case(s)) |") {
		t.Fatalf("no class section:\n%s", md)
	}
	run.Classes = nil
	if md := Summarise(run, rows).Markdown(); strings.Contains(md, "By failure class") {
		t.Fatal("a class section with no classes")
	}
}

func TestACaseClassIsAShortSlug(t *testing.T) {
	for class, ok := range map[string]bool{"date-order": true, "x1": true, "Date-Order": false, "has space": false, "-lead": false,
		strings.Repeat("a", 49): false} {
		extra := silentCase("classy", 5)
		extra["class"] = class
		_, err := Load(runnerSuite(t, extra))
		if (err == nil) != ok {
			t.Errorf("class %q: load error %v, want ok=%v", class, err, ok)
		}
	}
	extra := silentCase("classy", 5)
	extra["class"] = "date-order"
	s, err := Load(runnerSuite(t, extra))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Case("classy")
	if c.Class != "date-order" || caseClasses(s.Cases)["classy"] != "date-order" || len(caseClasses(s.Cases)) != 1 {
		t.Fatalf("class not kept: %+v", c.Class)
	}
}

// ---- ablation arms ----

// Every mechanism maps to a switch the agent already reads, with the value its code says means off.
func TestEveryAblationSwitchIsReadByTheAgent(t *testing.T) {
	root := filepath.Join("..", "..")
	for name, sw := range Mechanisms {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(sw.ReadIn)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(b), `"`+sw.Env+`"`) {
			t.Fatalf("%s: %s does not read %s", name, sw.ReadIn, sw.Env)
		}
		if _, clash := Unswitchable[name]; clash {
			t.Fatalf("%s is both switchable and not", name)
		}
	}
}

func TestAnAblationArmSetsOnlyItsSwitch(t *testing.T) {
	s, _, _, _, _ := newService(t, diligent)
	arms, err := s.checkArms([]Arm{{Name: "default+expert-playbooks", Kind: ArmModel, Expert: true, Off: "playbooks"}, {Name: "default", Kind: ArmModel}})
	if err != nil {
		t.Fatal(err)
	}
	if got := arms[0].switches(); len(got) != 1 || got[0] != "DECURIX_PLAYBOOKS=0" {
		t.Fatalf("switches: %v", got)
	}
	if got := arms[1].switches(); len(got) != 0 {
		t.Fatalf("a plain arm sets %v", got)
	}
	if !strings.Contains(armPins(arms[0]), "switched off: playbooks") {
		t.Fatalf("pins: %s", armPins(arms[0]))
	}
	// A mechanism with no switch is refused with why and what can be switched off; so is one nobody knows.
	_, err = s.checkArms([]Arm{{Name: "default+expert-replay", Kind: ArmModel, Expert: true, Off: "replay"}})
	if err == nil || !strings.Contains(err.Error(), "no switch that turns replay off") || !strings.Contains(err.Error(), "playbooks") {
		t.Fatalf("replay: %v", err)
	}
	if _, err := s.checkArms([]Arm{{Name: "default-telepathy", Kind: ArmModel, Off: "telepathy"}}); err == nil {
		t.Fatal("an unknown mechanism was accepted")
	}
	if _, err := s.checkArms([]Arm{{Name: "oracle", Kind: ArmOracle, Off: "playbooks"}}); err == nil {
		t.Fatal("a calibration arm switched something off")
	}
}

// ---- nothing the worker reads names an arm, the suite or a revision ----

func TestNothingTheWorkerSeesNamesTheArmSuiteOrRevision(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	const rev = "0123456789abcdef0123456789abcdef01234567"
	labels := []string{"default+expert@0123456789ab", "fast+expert-playbooks", "runner-test", rev, rev[:12]}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1,
		Arms: []Arm{{Name: labels[0], Kind: ArmModel, Expert: true, AgentTree: agentTree(t), AgentTreeRev: rev},
			{Name: labels[1], Kind: ArmModel, Model: "fast", Expert: true, Off: "playbooks"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.specs) != 2 || sb.specs[0].RunID != sb.specs[1].RunID {
		t.Fatalf("the arms' desktops are told apart by their run id: %d specs", len(sb.specs))
	}
	var seen []string // everything that reaches the desktop: its environment's inputs, the requests, the commands
	for _, sp := range sb.specs {
		seen = append(seen, sp.RunID, sp.Brain, sp.Model, sp.BrokerURL, string(sp.AgentTree), string(sp.Snapshot))
		seen = append(seen, sp.Hosts...)
		seen = append(seen, sp.Switches...)
	}
	for _, req := range sb.requests {
		for _, v := range req {
			seen = append(seen, v)
		}
	}
	for _, argv := range sb.execs {
		seen = append(seen, argv...)
	}
	for _, v := range seen {
		for _, label := range labels {
			if strings.Contains(v, label) {
				t.Fatalf("the worker can read %q in %q", label, v)
			}
		}
	}
	if len(sb.requests) == 0 {
		t.Fatal("no request was seen")
	}
}
