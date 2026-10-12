package httpapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// The task records as a file someone else can check: an accountant, an
// auditor, the person who owns the process. One row per task in CSV (what
// was asked, who did it, how it ended, whether the desktop's final check
// judged it, whether it counts as autonomous and why people stepped in), or
// every task in full - attempts, steps, interventions - in JSON.
//
//	GET /api/tasks/export?since=2026-09-01|days=7[&format=json]

// exportColumns are the CSV's header, in order.
var exportColumns = []string{
	"task_id", "created_at", "worker", "request", "type", "status", "attempts",
	"engine", "model", "checked", "autonomous", "stepped_in", "interventions",
	"approvals", "verdict", "verdict_note", "cost_usd", "tokens", "duration_seconds", "final_reply",
}

// taskExportHandler serves the export.
func (s *Server) taskExportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var since time.Time
	// days is the Tasks page's own period (1, 7, 30 or "all"), so its
	// Export button gives the tasks the page is showing.
	if v := r.URL.Query().Get("days"); v != "" && v != "all" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "days is a number of days, or all", http.StatusBadRequest)
			return
		}
		since = time.Now().AddDate(0, 0, -n)
	}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			http.Error(w, "since is a date like 2026-09-01", http.StatusBadRequest)
			return
		}
		since = t
	}

	list, err := s.Tasks.List(tasks.Filter{Since: since})
	if err != nil {
		jsonError(w, err)
		return
	}

	stamp := time.Now().Format("20060102-1504")
	w.Header().Set("Cache-Control", "no-store")

	if r.URL.Query().Get("format") == "json" {
		details := make([]tasks.Detail, 0, len(list))
		for _, sum := range list {
			d, err := s.Tasks.Get(sum.ID)
			if err != nil {
				jsonError(w, err)
				return
			}
			details = append(details, d)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="decurix-tasks-`+stamp+`.json"`)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"exported_at":         time.Now().UTC(),
			"since":               since,
			"autonomy_definition": tasks.AutonomyDefinition,
			"tasks":               details,
		})
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="decurix-tasks-`+stamp+`.csv"`)

	cw := csv.NewWriter(w)
	_ = cw.Write(exportColumns)
	for _, sum := range list {
		d, err := s.Tasks.Get(sum.ID)
		if err != nil {
			continue // a task deleted between the list and here is not one to report
		}
		_ = cw.Write(exportRow(sum, d))
	}
	cw.Flush()
}

// exportRow is one task's CSV row; the last attempt says how it ended.
func exportRow(sum tasks.Summary, d tasks.Detail) []string {
	var last tasks.Execution
	if n := len(d.Executions); n > 0 {
		last = d.Executions[n-1]
	}

	checked := "unknown"
	if last.Checked != nil {
		checked = map[bool]string{true: "yes", false: "no"}[*last.Checked]
	}

	used := ""
	if sum.Tokens != nil {
		used = fmt.Sprintf("%d", *sum.Tokens)
	}
	cost := ""
	if sum.CostUSD != nil {
		cost = fmt.Sprintf("%.4f", *sum.CostUSD)
	}

	row := []string{
		sum.ID, sum.CreatedAt.UTC().Format(time.RFC3339), sum.WorkerName, sum.Request, sum.Type, sum.Status,
		fmt.Sprint(sum.Executions), sum.Engine, sum.Model, checked, autonomy(sum),
		strings.Join(sum.SteppedIn, "; "), fmt.Sprint(sum.Interventions), fmt.Sprint(sum.Approvals),
		sum.Verdict, sum.VerdictNote, cost, used, fmt.Sprintf("%.0f", sum.Duration.Seconds()), last.FinalReply,
	}
	for i := range row {
		row[i] = spreadsheetSafe(row[i])
	}
	return row
}

// autonomy is the autonomous column: "not counted" for a demonstration or a
// lesson, which the autonomy report leaves out (tasks.CountsForAutonomy).
func autonomy(sum tasks.Summary) string {
	if !tasks.CountsForAutonomy(sum.Type) {
		return "not counted"
	}
	return fmt.Sprint(sum.Autonomous)
}

// spreadsheetSafe keeps a cell from being run as a formula when the CSV is
// opened in a spreadsheet: requests come from people and emails, and
// "=HYPERLINK(...)" typed by anyone would otherwise execute in the reader's
// Excel. A leading quote shows as text and changes nothing else.
func spreadsheetSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
