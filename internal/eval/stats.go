package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Z95 is the normal quantile for a two-sided 95% interval.
const Z95 = 1.96

// Wilson is the Wilson score interval for k successes in n trials at quantile z. With no trials it is (0, 1):
// nothing is known.
func Wilson(k, n int, z float64) (lo, hi float64) {
	if n <= 0 {
		return 0, 1
	}
	p := float64(k) / float64(n)
	nn := float64(n)
	z2 := z * z
	denom := 1 + z2/nn
	center := (p + z2/(2*nn)) / denom
	half := z * math.Sqrt(p*(1-p)/nn+z2/(4*nn*nn)) / denom
	return math.Max(0, center-half), math.Min(1, center+half)
}

// Cell is one case × arm of a table (or an arm's total). N, the rates and the means count the valid rows only: a row
// whose model changed under it (Invalid), whose model made no turn (NoTurn, run again), or whose case the runner could
// not run as written (Infra) says nothing of the worker, so it is counted apart and never in k/n or a Wilson bound
// (review EVAL-1, B4). A case run left out of one model arm is left out of every model arm (Paired). The token and
// minute figures leave out the runs that errored (Errored: an error, a timeout, the token budget), which still count
// 0 in k/n (docs/evals.md, "Bookkeeping").
type Cell struct {
	N                  int      `json:"n"`
	Pass               int      `json:"pass"`
	PassLo             float64  `json:"pass_lo"`
	FalseDone          int      `json:"false_done"`
	FalseDoneHi        float64  `json:"false_done_hi"`
	DoneUncheckedWrong int      `json:"done_unchecked_wrong"`
	QRequired          float64  `json:"q_required_mean"`
	QOther             float64  `json:"q_other_mean"`
	QDeclined          float64  `json:"q_declined_mean"`
	TokensMedian       int64    `json:"tokens_median"`
	TokensP90          int64    `json:"tokens_p90"`
	CachedShare        float64  `json:"cached_share"`
	MinutesMedian      float64  `json:"minutes_median"`
	Invalid            int      `json:"invalid"`
	Infra              int      `json:"infra"`
	Deployments        []string `json:"deployments"`
	// NoTurn counts rows the model made no turn in (each run again, by one rule for every arm).
	NoTurn int `json:"no_turn"`
	// Paired counts this arm's valid rows left out because the same case run was left out of another model arm.
	Paired int `json:"paired"`
	// Errored counts the N rows that ended in an error, a timeout or the token budget: 0 in k/n, left out of the
	// token and minute figures, which cover EfficiencyN rows.
	Errored     int `json:"errored"`
	EfficiencyN int `json:"efficiency_n"`
	// ProviderErrors is the calls the provider failed or never got, over every row of the cell.
	ProviderErrors int `json:"provider_errors"`
}

// ClassRow is one failure class's cells by arm: every case run of the cases that name the class (Case.Class).
type ClassRow struct {
	Class string          `json:"class"`
	Cases int             `json:"cases"`
	Arms  map[string]Cell `json:"arms"`
}

// TableRow is one case's cells by arm.
type TableRow struct {
	Case string          `json:"case"`
	Arms map[string]Cell `json:"arms"`
}

// Table is a run's report: a row per case, an arm per column, and a total per arm.
type Table struct {
	RunID     string          `json:"run_id"`
	Suite     string          `json:"suite"`
	SuiteSHA  string          `json:"suite_sha"`
	Repeats   int             `json:"repeats"`
	Status    string          `json:"status"`
	Note      string          `json:"note,omitempty"`
	Date      time.Time       `json:"date"`
	Arms      []Arm           `json:"arms"`
	Rows      []TableRow      `json:"rows"`
	Totals    map[string]Cell `json:"totals"`
	NotResult bool            `json:"not_a_result"`
	// Invalid counts the rows whose model changed under them; any makes the table say so first.
	Invalid int `json:"invalid"`
	// Infra counts the rows the runner could not run as written; any makes the result INCOMPLETE.
	Infra int `json:"infra"`
	// CarryState is a run that kept the desktop's state between a repeat's cases (Run.CarryState).
	CarryState bool `json:"carry_state"`
	// FromWorker, SnapshotSHA and WorkerTreeSHA are the run's (Run.FromWorker): the taught worker every case started from.
	FromWorker    string `json:"from_worker,omitempty"`
	SnapshotSHA   string `json:"snapshot_sha,omitempty"`
	WorkerTreeSHA string `json:"worker_tree_sha,omitempty"`
	// NoTurn counts rows the model made no turn in; Unscored the case runs still without a turn after every rerun
	// (any makes the table INVALID); Paired the valid rows left out because another model arm lost the same case run.
	NoTurn   int `json:"no_turn"`
	Unscored int `json:"unscored"`
	Paired   int `json:"paired"`
	// Classes is the per-failure-class section (cases that name a class only), classes sorted.
	Classes []ClassRow `json:"classes,omitempty"`
}

// slot is one case run: a case and a repeat (an arm's rows of it are its first run and any reruns).
type slot struct {
	caseID string
	repeat int
}

// Summarise builds a run's table from its rows, cases in the run's order and arms as the run named them.
func Summarise(run Run, rows []Row) Table {
	t := Table{RunID: run.ID, Suite: run.SuiteName, SuiteSHA: run.SuiteSHA, Repeats: run.Repeats, Status: run.Status, Note: run.Note,
		Date: run.CreatedAt, Arms: run.Arms, Rows: []TableRow{}, Totals: map[string]Cell{}, NotResult: run.Repeats < MinRepeats,
		CarryState: run.CarryState, FromWorker: run.FromWorker, SnapshotSHA: run.SnapshotSHA, WorkerTreeSHA: run.WorkerTreeSHA}
	cases := append([]string{}, run.Cases...)
	known := map[string]bool{}
	for _, c := range cases {
		known[c] = true
	}
	byCell := map[[2]string][]Row{}
	byArm := map[string][]Row{}
	for _, r := range rows {
		if !known[r.CaseID] {
			known[r.CaseID] = true
			cases = append(cases, r.CaseID)
		}
		byCell[[2]string{r.CaseID, r.Arm}] = append(byCell[[2]string{r.CaseID, r.Arm}], r)
		byArm[r.Arm] = append(byArm[r.Arm], r)
	}
	out, unscored := pairedOut(run.Arms, rows)
	t.Unscored = unscored
	isOut := func(r Row) bool { return out[r.Arm] != nil && out[r.Arm][slot{r.CaseID, r.Repeat}] }
	for _, c := range cases {
		tr := TableRow{Case: c, Arms: map[string]Cell{}}
		for _, a := range run.Arms {
			tr.Arms[a.Name] = cellOf(byCell[[2]string{c, a.Name}], isOut)
		}
		t.Rows = append(t.Rows, tr)
	}
	for _, a := range run.Arms {
		total := cellOf(byArm[a.Name], isOut)
		t.Totals[a.Name] = total
		t.Invalid += total.Invalid
		t.Infra += total.Infra
		t.NoTurn += total.NoTurn
		t.Paired += total.Paired
	}
	t.Classes = classRows(run, cases, byCell, isOut)
	return t
}

// pairedOut is, per model arm, the case runs (slots) whose valid rows are left out of its figures because the same case
// run was left out of another model arm: runs are never excluded from one arm only. A slot is left out of an arm when
// its last row there is invalid or infrastructure (a model change, no model turn after every rerun, a case the runner
// could not run); a slot an arm has not run yet leaves nothing out. The calibration arms are not compared, so never
// paired. It also counts the slots still without a model turn after every rerun.
func pairedOut(arms []Arm, rows []Row) (map[string]map[slot]bool, int) {
	model := map[string]bool{}
	for _, a := range arms {
		if a.Kind == ArmModel {
			model[a.Name] = true
		}
	}
	last := map[string]map[slot]Row{}
	for _, r := range rows {
		if !model[r.Arm] {
			continue
		}
		if last[r.Arm] == nil {
			last[r.Arm] = map[slot]Row{}
		}
		last[r.Arm][slot{r.CaseID, r.Repeat}] = r // rows come in the order stored: a rerun after its earlier runs
	}
	lost := map[slot]map[string]bool{} // slot -> the arms that lost it
	unscored := 0
	for arm, slots := range last {
		for sl, r := range slots {
			if r.Invalid || r.Infra {
				if lost[sl] == nil {
					lost[sl] = map[string]bool{}
				}
				lost[sl][arm] = true
				if r.invalidWhy() == InvalidNoTurn {
					unscored++
				}
			}
		}
	}
	out := map[string]map[slot]bool{}
	for sl, losers := range lost {
		for arm := range model {
			if losers[arm] {
				continue // its own rows are counted under their own reason
			}
			if out[arm] == nil {
				out[arm] = map[slot]bool{}
			}
			out[arm][sl] = true
		}
	}
	return out, unscored
}

// classRows is the per-failure-class section: for each class the run's cases name, every case run of its cases, by arm.
func classRows(run Run, cases []string, byCell map[[2]string][]Row, isOut func(Row) bool) []ClassRow {
	if len(run.Classes) == 0 {
		return nil
	}
	members := map[string][]string{}
	for _, c := range cases {
		if cl := run.Classes[c]; cl != "" {
			members[cl] = append(members[cl], c)
		}
	}
	names := make([]string, 0, len(members))
	for cl := range members {
		names = append(names, cl)
	}
	sort.Strings(names)
	var out []ClassRow
	for _, cl := range names {
		cr := ClassRow{Class: cl, Cases: len(members[cl]), Arms: map[string]Cell{}}
		for _, a := range run.Arms {
			var rows []Row
			for _, c := range members[cl] {
				rows = append(rows, byCell[[2]string{c, a.Name}]...)
			}
			cr.Arms[a.Name] = cellOf(rows, isOut)
		}
		out = append(out, cr)
	}
	return out
}

// MinRepeats is the fewest repeats whose rates are a result (docs/evals.md); fewer are said not to be one.
const MinRepeats = 5

// errored reports a row that ended without the worker's own answer, question or approval: an error, a timeout or the
// token budget. It counts 0 in k/n and is left out of the efficiency figures (its tokens were cut short, or ran away).
func errored(r Row) bool {
	return r.Observed == OutcomeError || r.Observed == OutcomeTimeout || r.Observed == OutcomeBudget
}

// cell summarises a group of rows: the valid ones, with the invalid and infrastructure ones counted apart.
func cell(all []Row) Cell { return cellOf(all, nil) }

// cellOf is cell, with out (nil: none) saying which valid rows are left out because another model arm lost their case
// run (pairedOut).
func cellOf(all []Row, out func(Row) bool) Cell {
	c := Cell{Deployments: []string{}}
	seen := map[string]bool{}
	var rows []Row
	for _, r := range all {
		for _, d := range r.Deployments {
			if !seen[d] {
				seen[d] = true
				c.Deployments = append(c.Deployments, d)
			}
		}
		c.ProviderErrors += r.ProviderErrors
		switch {
		case r.Invalid && r.invalidWhy() == InvalidNoTurn:
			c.NoTurn++
		case r.Invalid:
			c.Invalid++
		case r.Infra:
			c.Infra++
		case out != nil && out(r):
			c.Paired++
		default:
			rows = append(rows, r)
		}
	}
	sort.Strings(c.Deployments)
	c.N = len(rows)
	if len(rows) == 0 {
		c.PassLo, c.FalseDoneHi = 0, 1
		return c
	}
	var tokens []int64
	var minutes []float64
	var in, cached int64
	for _, r := range rows {
		if r.Passed {
			c.Pass++
		}
		if r.FalseDone {
			c.FalseDone++
		}
		if r.DoneUncheckedWrong {
			c.DoneUncheckedWrong++
		}
		c.QRequired += float64(r.QRequired)
		c.QOther += float64(r.QOther)
		c.QDeclined += float64(r.QDeclined)
		if errored(r) {
			c.Errored++
			continue
		}
		tokens = append(tokens, r.TokensIn+r.TokensOut)
		minutes = append(minutes, r.Minutes)
		in += r.TokensIn
		cached += r.TokensCached
	}
	n := float64(len(rows))
	c.QRequired, c.QOther, c.QDeclined = c.QRequired/n, c.QOther/n, c.QDeclined/n
	c.PassLo, _ = Wilson(c.Pass, c.N, Z95)
	_, c.FalseDoneHi = Wilson(c.FalseDone, c.N, Z95)
	c.EfficiencyN = len(tokens)
	if len(tokens) == 0 {
		return c
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i] < tokens[j] })
	sort.Float64s(minutes)
	c.TokensMedian = tokens[(len(tokens)-1)/2]
	if len(tokens)%2 == 0 {
		c.TokensMedian = (tokens[len(tokens)/2-1] + tokens[len(tokens)/2]) / 2
	}
	c.TokensP90 = tokens[int(math.Ceil(0.9*float64(len(tokens))))-1]
	c.MinutesMedian = minutes[(len(minutes)-1)/2]
	if len(minutes)%2 == 0 {
		c.MinutesMedian = (minutes[len(minutes)/2-1] + minutes[len(minutes)/2]) / 2
	}
	if in > 0 {
		c.CachedShare = float64(cached) / float64(in)
	}
	return c
}

// Markdown is the table for people: the run's pins first, then one section per measure with the arms as columns.
func (t Table) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Eval %s: %s\n\n", t.RunID, t.Suite)
	if t.Invalid > 0 || strings.Contains(t.Note, "a model changed") {
		fmt.Fprintf(&b, "**INVALID: a model changed during the run.** %d row(s) were served by another deployment than the "+
			"run pinned; they are left out of every rate below and counted apart.\n\n", t.Invalid)
	}
	if t.Unscored > 0 {
		fmt.Fprintf(&b, "**INVALID: %d case run(s) had no model turn after %d reruns** (the provider failed every call). "+
			"They are unscored, and the same case runs are left out of the other model arms.\n\n", t.Unscored, MaxCellReruns)
	}
	if t.Infra > 0 {
		fmt.Fprintf(&b, "**INCOMPLETE: %d case run(s) could not be run as written** (inputs, the mock or a delivery failed). "+
			"They are left out of k/n and shown beside it as \"+N infra\"; the rates cover fewer runs than were asked.\n\n", t.Infra)
	}
	if t.NotResult {
		fmt.Fprintf(&b, "**n=%d: not a result.** Fewer than %d repeats show what happened, not a rate.\n\n", t.Repeats, MinRepeats)
	}
	fmt.Fprintf(&b, "- Suite: `%s`, SHA-256 `%s`\n", t.Suite, t.SuiteSHA)
	fmt.Fprintf(&b, "- Date: %s; repeats: %d; status: %s\n", t.Date.UTC().Format("2006-01-02 15:04 UTC"), t.Repeats, t.Status)
	if t.CarryState {
		b.WriteString("- State: **carried from case to case** (carry_state): the desktop was reset before each repeat's " +
			"first case only, so a repeat's cases are a sequence, not independent samples\n")
	} else {
		b.WriteString("- State: reset to the snapshot before every case (independent samples)\n")
	}
	if t.FromWorker != "" {
		fmt.Fprintf(&b, "- Started from worker `%s` (agent tree %s, snapshot %s): every case began from its taught state\n",
			t.FromWorker, orDash(clipText(t.WorkerTreeSHA, 12)), orDash(clipText(t.SnapshotSHA, 12)))
	}
	if t.Note != "" {
		fmt.Fprintf(&b, "- Note: %s\n", t.Note)
	}
	fmt.Fprintf(&b, bookkeeping, MaxCellReruns)
	b.WriteString("- Arms:\n")
	for _, a := range t.Arms {
		served := "no calls"
		if deps := t.Totals[a.Name].Deployments; len(deps) > 0 {
			served = strings.Join(deps, ", ")
		}
		tot := t.Totals[a.Name]
		fmt.Fprintf(&b, "  - `%s`: %s; served by %s; provider errors %d; no-turn reruns %d\n", a.Name, armPins(a), served,
			tot.ProviderErrors, tot.NoTurn)
	}
	b.WriteString("\n")
	section := func(title string, value func(Cell) string) {
		fmt.Fprintf(&b, "## %s\n\n| case |", title)
		for _, a := range t.Arms {
			fmt.Fprintf(&b, " %s |", a.Name)
		}
		b.WriteString("\n|---|")
		for range t.Arms {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		for _, r := range t.Rows {
			fmt.Fprintf(&b, "| %s |", r.Case)
			for _, a := range t.Arms {
				fmt.Fprintf(&b, " %s |", value(r.Arms[a.Name]))
			}
			b.WriteString("\n")
		}
		b.WriteString("| **total** |")
		for _, a := range t.Arms {
			fmt.Fprintf(&b, " **%s** |", value(t.Totals[a.Name]))
		}
		b.WriteString("\n\n")
	}
	section("Pass (k/n, Wilson 95% lower bound)", func(c Cell) string {
		if c.N == 0 {
			return "-" + infraNote(c)
		}
		return fmt.Sprintf("%d/%d (lo %.2f)", c.Pass, c.N, c.PassLo) + infraNote(c)
	})
	section("False done (k/n, Wilson 95% upper bound); done unchecked and wrong", func(c Cell) string {
		if c.N == 0 {
			return "-" + infraNote(c)
		}
		return fmt.Sprintf("%d/%d (hi %.2f); %d", c.FalseDone, c.N, c.FalseDoneHi, c.DoneUncheckedWrong) + infraNote(c)
	})
	section("Questions per run (required / other / declined approvals)", func(c Cell) string {
		if c.N == 0 {
			return "-"
		}
		return fmt.Sprintf("%.1f / %.1f / %.1f", c.QRequired, c.QOther, c.QDeclined)
	})
	section("Tokens per run (median, p90; cached share of input), runs that errored left out", func(c Cell) string {
		if c.EfficiencyN == 0 {
			return "-" + erroredNote(c)
		}
		return fmt.Sprintf("%s, %s; %.0f%%", humanTokens(c.TokensMedian), humanTokens(c.TokensP90), 100*c.CachedShare) + erroredNote(c)
	})
	section("Minutes per run (median, runs that errored left out); rows left out: invalid, infrastructure, no model turn, paired", func(c Cell) string {
		if c.N == 0 && c.Invalid == 0 && c.Infra == 0 && c.NoTurn == 0 && c.Paired == 0 {
			return "-"
		}
		minutes := "-"
		if c.EfficiencyN > 0 {
			minutes = fmt.Sprintf("%.1f", c.MinutesMedian)
		}
		return fmt.Sprintf("%s; %d, %d, %d, %d", minutes, c.Invalid, c.Infra, c.NoTurn, c.Paired)
	})
	if len(t.Classes) > 0 {
		fmt.Fprintf(&b, "## By failure class (pass k/n, Wilson 95%% lower bound; false done)\n\nA fix names the class it moves "+
			"and must move its trap cases.\n\n| class |")
		for _, a := range t.Arms {
			fmt.Fprintf(&b, " %s |", a.Name)
		}
		b.WriteString("\n|---|")
		for range t.Arms {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		for _, cr := range t.Classes {
			fmt.Fprintf(&b, "| %s (%d case(s)) |", cr.Class, cr.Cases)
			for _, a := range t.Arms {
				c := cr.Arms[a.Name]
				if c.N == 0 {
					fmt.Fprintf(&b, " -%s |", infraNote(c))
					continue
				}
				fmt.Fprintf(&b, " %d/%d (lo %.2f); fd %d |", c.Pass, c.N, c.PassLo, c.FalseDone)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// bookkeeping (a format: the reruns) is the table head's statement of the rules every figure below keeps (docs/evals.md, "Bookkeeping").
const bookkeeping = "- Bookkeeping: a case run whose model made no turn (every call failed at the provider) is invalid and run " +
	"again, at most %d more times, by one rule for every arm; a case run left out of one model arm is left out of every " +
	"model arm; a run that errored (error, timeout, token budget) counts 0 in pass and is left out of tokens and " +
	"minutes; efficiency is compared in tokens, never in money\n"

// erroredNote is the count of a cell's runs left out of its efficiency figures because they errored.
func erroredNote(c Cell) string {
	if c.Errored == 0 {
		return ""
	}
	return fmt.Sprintf(" (-%d errored)", c.Errored)
}

// infraNote is the count of a cell's runs left out as infrastructure, shown beside its k/n.
func infraNote(c Cell) string {
	if c.Infra == 0 {
		return ""
	}
	return fmt.Sprintf(" +%d infra", c.Infra)
}

// armPins says what an arm pinned: its kind and model as resolved, the agent tree, the image and the expert.
func armPins(a Arm) string {
	var parts []string
	switch a.Kind {
	case ArmModel:
		dep := a.Deployment
		if dep == "" {
			dep = "not resolved"
		}
		served := ""
		if a.Served != "" && a.Served != dep {
			served = fmt.Sprintf(" (answers say %s)", a.Served)
		}
		parts = append(parts, fmt.Sprintf("%s %s → %s%s", a.Provider, a.Model, dep, served))
		if a.Expert {
			parts = append(parts, "with the expert ("+orDash(a.ExpertDeployment)+")")
		} else {
			parts = append(parts, "no expert")
		}
	default:
		parts = append(parts, a.Kind+" (no model)")
	}
	if sw, ok := offSwitch(a.Off); ok && sw.Env != "" {
		parts = append(parts, fmt.Sprintf("switched off: %s (%s=%s)", sw.What, sw.Env, sw.Value))
	}
	tree := "image"
	if a.AgentTreeRev != "" {
		tree = a.AgentTreeRev
		if a.AgentTreeSHA != "" {
			tree += " (" + clipText(a.AgentTreeSHA, 12) + ")"
		}
	}
	parts = append(parts, "agent tree "+tree, "image "+orDash(clipText(a.Image, 19)))
	return strings.Join(parts, "; ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
