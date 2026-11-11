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
// whose model changed under it (Invalid) or whose case the runner could not run as written (Infra) says nothing of
// the worker, so it is counted apart and never in k/n or a Wilson bound (review EVAL-1, B4).
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
	for _, c := range cases {
		tr := TableRow{Case: c, Arms: map[string]Cell{}}
		for _, a := range run.Arms {
			tr.Arms[a.Name] = cell(byCell[[2]string{c, a.Name}])
		}
		t.Rows = append(t.Rows, tr)
	}
	for _, a := range run.Arms {
		t.Totals[a.Name] = cell(byArm[a.Name])
		t.Invalid += t.Totals[a.Name].Invalid
		t.Infra += t.Totals[a.Name].Infra
	}
	return t
}

// MinRepeats is the fewest repeats whose rates are a result (docs/evals.md); fewer are said not to be one.
const MinRepeats = 5

// cell summarises a group of rows: the valid ones, with the invalid and infrastructure ones counted apart.
func cell(all []Row) Cell {
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
		switch {
		case r.Invalid:
			c.Invalid++
		case r.Infra:
			c.Infra++
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
		tokens = append(tokens, r.TokensIn+r.TokensOut)
		minutes = append(minutes, r.Minutes)
		in += r.TokensIn
		cached += r.TokensCached
	}
	n := float64(len(rows))
	c.QRequired, c.QOther, c.QDeclined = c.QRequired/n, c.QOther/n, c.QDeclined/n
	c.PassLo, _ = Wilson(c.Pass, c.N, Z95)
	_, c.FalseDoneHi = Wilson(c.FalseDone, c.N, Z95)
	sort.Slice(tokens, func(i, j int) bool { return tokens[i] < tokens[j] })
	sort.Float64s(minutes)
	c.TokensMedian = tokens[(len(tokens)-1)/2]
	if len(tokens)%2 == 0 {
		c.TokensMedian = (tokens[len(tokens)/2-1] + tokens[len(tokens)/2]) / 2
	}
	c.TokensP90 = tokens[int(math.Ceil(0.9*n))-1]
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
	if t.Invalid > 0 || strings.Contains(t.Note, "invalid") {
		fmt.Fprintf(&b, "**INVALID: a model changed during the run.** %d row(s) were served by another deployment than the "+
			"run pinned; they are left out of every rate below and counted apart.\n\n", t.Invalid)
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
	b.WriteString("- Arms:\n")
	for _, a := range t.Arms {
		served := "no calls"
		if deps := t.Totals[a.Name].Deployments; len(deps) > 0 {
			served = strings.Join(deps, ", ")
		}
		fmt.Fprintf(&b, "  - `%s`: %s; served by %s\n", a.Name, armPins(a), served)
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
	section("Tokens per run (median, p90; cached share of input)", func(c Cell) string {
		if c.N == 0 {
			return "-"
		}
		return fmt.Sprintf("%s, %s; %.0f%%", humanTokens(c.TokensMedian), humanTokens(c.TokensP90), 100*c.CachedShare)
	})
	section("Minutes per run (median); rows left out: invalid, infrastructure", func(c Cell) string {
		if c.N == 0 && c.Invalid == 0 && c.Infra == 0 {
			return "-"
		}
		minutes := "-"
		if c.N > 0 {
			minutes = fmt.Sprintf("%.1f", c.MinutesMedian)
		}
		return fmt.Sprintf("%s; %d, %d", minutes, c.Invalid, c.Infra)
	})
	return b.String()
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
