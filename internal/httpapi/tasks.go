package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// maxVerdictNoteBytes bounds what a human can attach to a "wrong" verdict.
const maxVerdictNoteBytes = 2048

// maxListedTasks bounds the task list the dashboard renders.
const maxListedTasks = 200

// windows are the report periods the dashboard offers, by query value.
var windows = []webui.WindowOption{
	{Value: "1", Label: "24 hours", Phrase: "in the last 24 hours"},
	{Value: "7", Label: "7 days", Phrase: "this week"},
	{Value: "30", Label: "30 days", Phrase: "in the last 30 days"},
	{Value: "all", Label: "All time", Phrase: "overall"},
}

// reportWindow reads the ?days= period, defaulting to a week, and returns its
// canonical value and start time (zero for all time).
func reportWindow(r *http.Request) (string, time.Time) {
	value := strings.TrimSpace(r.URL.Query().Get("days"))

	switch value {
	case "all":
		return value, time.Time{}
	case "1", "30":
	default:
		value = "7"
	}

	days, _ := strconv.Atoi(value)

	return value, time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
}

// taskFilter reads the list filters shared by the page and the API.
func taskFilter(r *http.Request, since time.Time) tasks.Filter {
	query := r.URL.Query()

	return tasks.Filter{
		Since:    since,
		WorkerID: desktop.SanitizeID(strings.TrimSpace(query.Get("worker"))),
		Engine:   strings.TrimSpace(query.Get("engine")),
		Type:     strings.TrimSpace(query.Get("type")),
	}
}

// tasksPageHandler renders the task records dashboard: autonomy by engine,
// worker and task type, why humans stepped in, and the recent tasks.
func (s *Server) tasksPageHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/tasks" {
		http.NotFound(w, r)
		return
	}

	window, since := reportWindow(r)

	filter := taskFilter(r, since)
	filter.Limit = maxListedTasks

	report, list, err := s.Tasks.ReportAndList(filter)
	if err != nil {
		http.Error(w, "could not read the task records", http.StatusInternalServerError)
		return
	}

	data := webui.TasksPageData{
		Report: report,
		Tasks:  list,
		Filter: filter,
	}

	for _, option := range windows {
		option.Active = option.Value == window
		if option.Active {
			data.Window = option
		}
		data.Windows = append(data.Windows, option)
	}

	if err := s.Web.RenderTasks(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// taskPageHandler renders one task record: every attempt, step, screenshot and
// intervention, and the verdict control.
func (s *Server) taskPageHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/tasks/"), "/")

	if !tasks.ValidID(id) {
		http.NotFound(w, r)
		return
	}

	detail, err := s.Tasks.Get(id)
	if errors.Is(err, tasks.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not read the task record", http.StatusInternalServerError)
		return
	}

	if err := s.Web.RenderTask(w, webui.TaskPageData{Task: detail}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// tasksAPIHandler lists task summaries as JSON, filtered by ?days=, ?worker=,
// ?engine= and ?type=, newest first, at most ?limit= of them.
func (s *Server) tasksAPIHandler(w http.ResponseWriter, r *http.Request) {
	_, since := reportWindow(r)
	filter := taskFilter(r, since)

	filter.Limit = maxListedTasks
	if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && limit > 0 && limit < filter.Limit {
		filter.Limit = limit
	}

	list, err := s.Tasks.List(filter)
	if err != nil {
		jsonError(w, err)
		return
	}

	if list == nil {
		list = []tasks.Summary{}
	}

	jsonResponse(w, list)
}

// taskAPIHandler returns one task record as JSON.
func (s *Server) taskAPIHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	if !tasks.ValidID(id) {
		jsonStatusError(w, http.StatusNotFound, "no such task")
		return
	}

	detail, err := s.Tasks.Get(id)
	if errors.Is(err, tasks.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such task")
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, detail)
}

// taskReportAPIHandler returns the autonomy report for ?days= as JSON.
func (s *Server) taskReportAPIHandler(w http.ResponseWriter, r *http.Request) {
	_, since := reportWindow(r)

	report, err := s.Tasks.Report(since)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, report)
}

// taskVerdictHandler records a human's one-tap verdict on a task's answer -
// "correct", "wrong", or "" to take it back - with an optional note, and
// shows it in that desktop's chat on every device.
func (s *Server) taskVerdictHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimSpace(r.FormValue("task_id"))
	verdict := strings.TrimSpace(r.FormValue("verdict"))
	note := r.FormValue("note")

	if !tasks.ValidID(id) {
		jsonStatusError(w, http.StatusNotFound, "no such task")
		return
	}

	switch verdict {
	case tasks.VerdictCorrect, tasks.VerdictWrong, "":
	default:
		jsonStatusError(w, http.StatusBadRequest, "verdict must be correct or wrong")
		return
	}

	if len(note) > maxVerdictNoteBytes {
		jsonStatusError(w, http.StatusBadRequest, "note is too long")
		return
	}

	task, err := s.Tasks.SetVerdict(id, verdict, note)
	if errors.Is(err, tasks.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such task")
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	s.Chat.SetVerdict(task.WorkerID, task.ID, task.Verdict)

	jsonResponse(w, task)
}
