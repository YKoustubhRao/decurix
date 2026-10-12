package tasks

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// How autonomy is counted. It is stated once, here, and the dashboard repeats
// it word for word, because a rate nobody can reproduce is not worth reading.
//
// A task counts towards a rate once it is settled: it has ended, or a human has
// already had to step in. It is autonomous when its only attempt ended done,
// nobody had a query or asked for approval along the way, and no human marked
// the answer wrong. An approval counts as stepping in: it is the agent doing
// the right thing, but it is still a person being paged, and the reasons table
// keeps approvals apart from being stuck.
const AutonomyDefinition = "A task is autonomous when its only attempt ended done, it never had a query or asked a human for approval, and nobody marked the answer wrong. Tasks still running with no human involvement yet are not counted."

var engineLabels = map[string]string{
	"ollama": "Ollama",
	"claude": "Claude Code",
	"thebot": "TheBot",
	// A recording run again by the desktop itself, with no model at all.
	"replay": "Replay (no model)",
	// A person doing a task while the desktop recorded them.
	"human": "Recorded by a person",
	// A desktop being taught a skill, and one doing a skill it was taught:
	// both with no model.
	"learn": "Teaching (no model)",
	"skill": "Skill (no model)",
	// A question put to a web app (ChatGPT, Google, ...) in the desktop's
	// browser and its answer read off the page (agent ask_task.py).
	"web": "Web app (no model)",
	// A request done by a script the desktop wrote for itself from work it
	// had done by hand (agent tool_task.py, playbooks.py): no engine turn.
	"tool": "Own tool",
}

// EngineLabel is the human-readable name of an engine.
func EngineLabel(engine string) string {
	if label, ok := engineLabels[engine]; ok {
		return label
	}

	if engine == "" {
		return "unknown"
	}

	return engine
}

// Summary is a task with the figures the list and the report need.
type Summary struct {
	Task

	Model          string        `json:"model"`
	Executions     int           `json:"executions"`
	Interventions  int           `json:"interventions"`
	Approvals      int           `json:"approvals"`
	Errors         int           `json:"errors"`
	Steps          int           `json:"steps"`
	ScreenActions  int           `json:"screen_actions"`
	Duration       time.Duration `json:"duration_ns"`
	WaitingOnHuman time.Duration `json:"waiting_on_human_ns"`
	CostUSD        *float64      `json:"cost_usd"`
	// Tokens is what the task's model calls read and wrote in all, over every
	// attempt; nil when no engine reported a count. A hosted model's cost is
	// on the provider's bill and never reported here, so this is the figure
	// that shows whether a worker is getting cheaper at its work.
	Tokens     *int64 `json:"tokens"`
	Settled    bool   `json:"settled"`
	Autonomous bool   `json:"autonomous"`

	// SteppedIn lists, as reason keys, every time a human had to act on this
	// task: each intervention, each failed attempt, a stop, a wrong verdict.
	SteppedIn []string `json:"stepped_in"`

	// examples holds, per reason key, the words behind that stepping-in.
	examples map[string]string
}

// Filter narrows a task list. Zero values match everything.
type Filter struct {
	Since    time.Time
	WorkerID string
	Engine   string
	Type     string
	Limit    int
}

// CountsForAutonomy reports whether a task of this type is an attempt an
// engine made. A demonstration was done by a person while it was recorded,
// and a lesson is a person being asked about one: neither is counted.
func CountsForAutonomy(taskType string) bool {
	return taskType != TypeDemonstration && taskType != TypeTeaching
}

// ReportAndList computes the autonomy report and filtered task list in a single
// database pass over the tasks created since a time.
func (s *Store) ReportAndList(filter Filter) (Report, []Summary, error) {
	summaries, err := s.summaries(filter.Since)
	if err != nil {
		return Report{}, nil, err
	}

	report := reportFromSummaries(filter.Since, summaries)
	list := filterSummaries(summaries, filter)
	return report, list, nil
}

// List returns task summaries, newest first.
func (s *Store) List(filter Filter) ([]Summary, error) {
	summaries, err := s.summaries(filter.Since)
	if err != nil {
		return nil, err
	}

	return filterSummaries(summaries, filter), nil
}

func filterSummaries(summaries []Summary, filter Filter) []Summary {
	var kept []Summary

	for _, summary := range summaries {
		if filter.WorkerID != "" && summary.WorkerID != filter.WorkerID ||
			filter.Engine != "" && summary.Engine != filter.Engine ||
			filter.Type != "" && summary.Type != filter.Type {
			continue
		}

		kept = append(kept, summary)
	}

	if filter.Limit > 0 && len(kept) > filter.Limit {
		kept = kept[:filter.Limit]
	}

	return kept
}

// summaries loads every task created since a time, with its figures.
func (s *Store) summaries(since time.Time) ([]Summary, error) {
	sinceNS := sqlitedb.ToNS(since)

	rows, err := s.db.Query(
		`SELECT `+taskColumns+` FROM tasks t LEFT JOIN workers w ON w.id = t.worker_id
		 WHERE t.created_ns >= ? ORDER BY t.created_ns DESC`, sinceNS,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks: listing: %w", err)
	}

	var summaries []Summary

	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("tasks: listing: %w", err)
		}

		summaries = append(summaries, Summary{Task: task, SteppedIn: []string{}, examples: map[string]string{}})
	}
	rows.Close()

	index := make(map[string]int, len(summaries))
	for i := range summaries {
		index[summaries[i].ID] = i
	}

	executions, err := s.loadExecutions(`WHERE t.created_ns >= ?`, sinceNS)
	if err != nil {
		return nil, err
	}

	interventions, err := s.loadInterventions(`WHERE t.created_ns >= ?`, sinceNS)
	if err != nil {
		return nil, err
	}

	counts, err := s.db.Query(
		`SELECT e.task_id, COUNT(*), COALESCE(SUM(CASE WHEN s.action != '' THEN 1 ELSE 0 END), 0)
		 FROM execution_steps s JOIN executions e ON e.id = s.execution_id JOIN tasks t ON t.id = e.task_id
		 WHERE t.created_ns >= ? GROUP BY e.task_id`, sinceNS,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks: counting steps: %w", err)
	}

	for counts.Next() {
		var taskID string
		var steps, actions int

		if err := counts.Scan(&taskID, &steps, &actions); err != nil {
			counts.Close()
			return nil, fmt.Errorf("tasks: counting steps: %w", err)
		}

		if i, ok := index[taskID]; ok {
			summaries[i].Steps = steps
			summaries[i].ScreenActions = actions
		}
	}
	counts.Close()

	now := s.now()
	executionTask := make(map[string]int, len(executions))

	for _, execution := range executions {
		i, ok := index[execution.TaskID]
		if !ok {
			continue
		}

		executionTask[execution.ID] = i
		summary := &summaries[i]

		summary.Executions++
		summary.Model = execution.Model

		switch {
		case !execution.EndedAt.IsZero():
			summary.Duration += execution.EndedAt.Sub(execution.StartedAt)
		case execution.Status == StatusRunning || execution.Status == StatusWaiting:
			summary.Duration += now.Sub(execution.StartedAt)
		}

		if execution.CostUSD != nil {
			total := *execution.CostUSD
			if summary.CostUSD != nil {
				total += *summary.CostUSD
			}
			summary.CostUSD = &total
		}

		if execution.InputTokens != nil || execution.OutputTokens != nil {
			var total int64
			if summary.Tokens != nil {
				total = *summary.Tokens
			}
			if execution.InputTokens != nil {
				total += *execution.InputTokens
			}
			if execution.OutputTokens != nil {
				total += *execution.OutputTokens
			}
			summary.Tokens = &total
		}

		switch execution.Status {
		case StatusError:
			summary.Errors++
			reason := ErrorReason(execution.FinalReply)
			summary.SteppedIn = append(summary.SteppedIn, reason)
			summary.examples[reason] = execution.FinalReply
		case StatusAbandoned:
			summary.SteppedIn = append(summary.SteppedIn, ReasonStopped)
			summary.examples[ReasonStopped] = execution.FinalReply
		}
	}

	for _, intervention := range interventions {
		i, ok := executionTask[intervention.ExecutionID]
		if !ok {
			continue
		}

		summary := &summaries[i]

		summary.Interventions++
		if intervention.Kind == InterventionApproval || IsApprovalReason(intervention.Reason) {
			summary.Approvals++
		}

		summary.SteppedIn = append(summary.SteppedIn, intervention.Reason)
		summary.examples[intervention.Reason] = intervention.Request

		waitedUntil := intervention.RepliedAt
		if waitedUntil.IsZero() {
			waitedUntil = now
		}
		summary.WaitingOnHuman += waitedUntil.Sub(intervention.RequestedAt)
	}

	for i := range summaries {
		summary := &summaries[i]

		if summary.Verdict == VerdictWrong {
			summary.SteppedIn = append(summary.SteppedIn, ReasonWrong)
			summary.examples[ReasonWrong] = summary.VerdictNote
		}

		live := summary.Status == StatusQueued || summary.Status == StatusRunning || summary.Status == StatusWaiting
		summary.Settled = !live || len(summary.SteppedIn) > 0
		summary.Autonomous = summary.Status == StatusDone && summary.Executions == 1 &&
			summary.Interventions == 0 && summary.Verdict != VerdictWrong
	}

	return summaries, nil
}

// Group is one row of the autonomy report: an engine, a worker or a task type.
type Group struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Engine string   `json:"engine,omitempty"`
	Models []string `json:"models,omitempty"`

	Tasks         int     `json:"tasks"`
	Settled       int     `json:"settled"`
	Autonomous    int     `json:"autonomous"`
	Rate          float64 `json:"rate"`
	Interventions int     `json:"interventions"`
	Approvals     int     `json:"approvals"`
	Errors        int     `json:"errors"`
	Correct       int     `json:"correct"`
	Wrong         int     `json:"wrong"`
	Unverified    int     `json:"unverified"`

	MedianSeconds float64  `json:"median_seconds"`
	CostUSD       *float64 `json:"cost_usd"`
	// Tokens is the group's reported token use; TokenTasks how many of its
	// tasks reported one, so a mean is over those and not over all.
	Tokens     *int64 `json:"tokens"`
	TokenTasks int    `json:"token_tasks"`
	TopReason  string `json:"top_reason,omitempty"`

	durations []time.Duration
	reasons   map[string]int
	models    map[string]bool
}

// RatePercent is the autonomy rate as a whole percentage.
func (g Group) RatePercent() int {
	return int(g.Rate*100 + 0.5)
}

// TopReasonLabel names the group's most common reason for stepping in.
func (g Group) TopReasonLabel() string {
	return ReasonLabel(g.TopReason)
}

// Median is the group's median time per settled task, for display.
func (g Group) Median() string {
	return FormatDuration(time.Duration(g.MedianSeconds * float64(time.Second)))
}

// Cost is the group's reported spend, for display.
func (g Group) Cost() string {
	return FormatCost(g.CostUSD)
}

// TokensUsed is the group's reported token use, for display.
func (g Group) TokensUsed() string {
	return FormatTokens(g.Tokens)
}

// TokensPerTask is the mean token use of the group's tasks that reported one, for display.
func (g Group) TokensPerTask() string {
	if g.Tokens == nil || g.TokenTasks == 0 {
		return FormatTokens(nil)
	}
	mean := *g.Tokens / int64(g.TokenTasks)
	return FormatTokens(&mean)
}

// Reason is one row of "why humans stepped in".
type Reason struct {
	Key      string          `json:"key"`
	Label    string          `json:"label"`
	Approval bool            `json:"approval"`
	Count    int             `json:"count"`
	Share    float64         `json:"share"`
	ByEngine []EngineCount   `json:"by_engine"`
	Examples []ReasonExample `json:"examples"`
}

// SharePercent is the reason's share as a whole percentage.
func (r Reason) SharePercent() int {
	return int(r.Share*100 + 0.5)
}

// EngineCount is how often one engine hit a reason.
type EngineCount struct {
	Engine string `json:"engine"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
}

// ReasonExample is one recent occurrence of a reason, to show what it looked like.
type ReasonExample struct {
	TaskID  string `json:"task_id"`
	Worker  string `json:"worker"`
	Engine  string `json:"engine"`
	Request string `json:"request"`
	Words   string `json:"words"`
}

// Report is the autonomy dashboard's data for one window of time.
type Report struct {
	Since      time.Time `json:"since"`
	Definition string    `json:"definition"`

	Totals   Group    `json:"totals"`
	ByEngine []Group  `json:"by_engine"`
	ByWorker []Group  `json:"by_worker"`
	ByType   []Group  `json:"by_type"`
	Reasons  []Reason `json:"reasons"`

	// Best is the engine with the highest autonomy rate, when any engine has
	// a settled task; StepIns is how many times a human stepped in, overall.
	Best    *Group `json:"best,omitempty"`
	StepIns int    `json:"step_ins"`
}

// maxExamples bounds the sample occurrences shown per reason.
const maxExamples = 3

// Report computes autonomy by engine, worker and task type, and the reasons
// humans had to step in, over tasks created since a time.
func (s *Store) Report(since time.Time) (Report, error) {
	summaries, err := s.summaries(since)
	if err != nil {
		return Report{}, err
	}

	return reportFromSummaries(since, summaries), nil
}

func reportFromSummaries(since time.Time, summaries []Summary) Report {
	report := Report{
		Since:      since,
		Definition: AutonomyDefinition,
		Totals:     newGroup("all", "All workers"),
	}

	engines := map[string]*Group{}
	workers := map[string]*Group{}
	types := map[string]*Group{}
	reasons := map[string]*Reason{}
	reasonEngines := map[string]map[string]int{}

	group := func(groups map[string]*Group, key, label string) *Group {
		if existing, ok := groups[key]; ok {
			return existing
		}

		created := newGroup(key, label)
		groups[key] = &created

		return &created
	}

	for _, summary := range summaries {
		if !CountsForAutonomy(summary.Type) {
			continue
		}

		engine := group(engines, summary.Engine, EngineLabel(summary.Engine))
		worker := group(workers, summary.WorkerID, summary.WorkerName)
		worker.Engine = summary.Engine
		taskType := group(types, summary.Type, TypeLabel(summary.Type))

		for _, g := range []*Group{&report.Totals, engine, worker, taskType} {
			g.add(summary)
		}

		// Summaries are newest first, so the first examples kept are the most recent.
		for _, key := range summary.SteppedIn {
			report.StepIns++

			reason, ok := reasons[key]
			if !ok {
				reason = &Reason{Key: key, Label: ReasonLabel(key), Approval: IsApprovalReason(key)}
				reasons[key] = reason
				reasonEngines[key] = map[string]int{}
			}

			reason.Count++
			reasonEngines[key][summary.Engine]++

			if len(reason.Examples) < maxExamples && !hasExample(reason.Examples, summary.ID) {
				reason.Examples = append(reason.Examples, ReasonExample{
					TaskID:  summary.ID,
					Worker:  summary.WorkerName,
					Engine:  summary.Engine,
					Request: clipText(summary.Request, 140),
					Words:   clipText(summary.examples[key], 220),
				})
			}
		}
	}

	report.Totals.finish()

	for _, g := range engines {
		g.finish()
		report.ByEngine = append(report.ByEngine, *g)
	}

	for _, g := range workers {
		g.finish()
		report.ByWorker = append(report.ByWorker, *g)
	}

	for _, g := range types {
		g.finish()
		report.ByType = append(report.ByType, *g)
	}

	sort.Slice(report.ByEngine, func(i, j int) bool { return betterRate(report.ByEngine[i], report.ByEngine[j]) })
	sort.Slice(report.ByWorker, func(i, j int) bool { return betterRate(report.ByWorker[i], report.ByWorker[j]) })
	sort.Slice(report.ByType, func(i, j int) bool {
		if report.ByType[i].Settled != report.ByType[j].Settled {
			return report.ByType[i].Settled > report.ByType[j].Settled
		}
		return report.ByType[i].Key < report.ByType[j].Key
	})

	for i := range report.ByEngine {
		if report.ByEngine[i].Settled > 0 {
			best := report.ByEngine[i]
			report.Best = &best
			break
		}
	}

	for key, reason := range reasons {
		if report.StepIns > 0 {
			reason.Share = float64(reason.Count) / float64(report.StepIns)
		}

		for engine, count := range reasonEngines[key] {
			reason.ByEngine = append(reason.ByEngine, EngineCount{Engine: engine, Label: EngineLabel(engine), Count: count})
		}

		sort.Slice(reason.ByEngine, func(i, j int) bool {
			if reason.ByEngine[i].Count != reason.ByEngine[j].Count {
				return reason.ByEngine[i].Count > reason.ByEngine[j].Count
			}
			return reason.ByEngine[i].Engine < reason.ByEngine[j].Engine
		})

		report.Reasons = append(report.Reasons, *reason)
	}

	sort.Slice(report.Reasons, func(i, j int) bool {
		if report.Reasons[i].Count != report.Reasons[j].Count {
			return report.Reasons[i].Count > report.Reasons[j].Count
		}
		return report.Reasons[i].Key < report.Reasons[j].Key
	})

	return report
}

func newGroup(key, label string) Group {
	return Group{Key: key, Label: label, reasons: map[string]int{}, models: map[string]bool{}}
}

func (g *Group) add(summary Summary) {
	g.Tasks++

	if summary.Model != "" {
		g.models[summary.Model] = true
	}

	if summary.CostUSD != nil {
		total := *summary.CostUSD
		if g.CostUSD != nil {
			total += *g.CostUSD
		}
		g.CostUSD = &total
	}

	if summary.Tokens != nil {
		total := *summary.Tokens
		if g.Tokens != nil {
			total += *g.Tokens
		}
		g.Tokens = &total
		g.TokenTasks++
	}

	if !summary.Settled {
		return
	}

	g.Settled++
	g.Interventions += summary.Interventions
	g.Approvals += summary.Approvals
	g.Errors += summary.Errors

	if summary.Autonomous {
		g.Autonomous++

		if summary.Verdict == "" {
			g.Unverified++
		}
	}

	for _, key := range summary.SteppedIn {
		g.reasons[key]++
	}

	switch summary.Verdict {
	case VerdictCorrect:
		g.Correct++
	case VerdictWrong:
		g.Wrong++
	}

	if summary.Duration > 0 {
		g.durations = append(g.durations, summary.Duration)
	}
}

func (g *Group) finish() {
	if g.Settled > 0 {
		g.Rate = float64(g.Autonomous) / float64(g.Settled)
	}

	if len(g.durations) > 0 {
		sort.Slice(g.durations, func(i, j int) bool { return g.durations[i] < g.durations[j] })

		middle := len(g.durations) / 2
		median := g.durations[middle]

		if len(g.durations)%2 == 0 {
			median = (g.durations[middle-1] + g.durations[middle]) / 2
		}

		g.MedianSeconds = median.Seconds()
	}

	best := 0
	for key, count := range g.reasons {
		if count > best || count == best && key < g.TopReason {
			best = count
			g.TopReason = key
		}
	}

	for model := range g.models {
		g.Models = append(g.Models, model)
	}
	sort.Strings(g.Models)
}

// betterRate orders groups by autonomy rate, then by how much evidence backs
// it, so one lucky task does not outrank a hundred good ones at the same rate.
func betterRate(a, b Group) bool {
	if (a.Settled > 0) != (b.Settled > 0) {
		return a.Settled > 0
	}

	if a.Rate != b.Rate {
		return a.Rate > b.Rate
	}

	if a.Settled != b.Settled {
		return a.Settled > b.Settled
	}

	return a.Key < b.Key
}

func hasExample(examples []ReasonExample, taskID string) bool {
	for _, example := range examples {
		if example.TaskID == taskID {
			return true
		}
	}

	return false
}

func clipText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")

	if len([]rune(text)) <= limit {
		return text
	}

	return string([]rune(text)[:limit]) + "…"
}

// FormatDuration renders a task's time for people: "8s", "4m 12s", "2h 5m".
func FormatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// FormatTokens renders a reported token count; nil means none was reported.
func FormatTokens(tokens *int64) string {
	switch {
	case tokens == nil:
		return "—"
	case *tokens < 1000:
		return fmt.Sprintf("%d", *tokens)
	case *tokens < 1000000:
		return fmt.Sprintf("%.1fk", float64(*tokens)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(*tokens)/1000000)
	}
}

// FormatCost renders a reported spend; nil means none was reported.
func FormatCost(cost *float64) string {
	switch {
	case cost == nil:
		return "—"
	case *cost == 0:
		return "$0"
	case *cost < 0.01:
		return "<$0.01"
	default:
		return fmt.Sprintf("$%.2f", *cost)
	}
}
