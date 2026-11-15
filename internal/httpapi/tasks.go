package httpapi

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
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
		Purge:  !isGated(r),
		Search: !isGated(r) && s.RunSearch != nil,
	}

	for _, option := range windows {
		option.Active = option.Value == window
		if option.Active {
			data.Window = option
		}
		data.Windows = append(data.Windows, option)
	}

	if err := s.Web.RenderTasks(w, data); err != nil {
		internalError(w, "tasks page", err)
	}
}

// taskPageHandler renders one task record (/tasks/<id>): every attempt, step,
// screenshot and intervention, and the verdict control. /tasks/<id>/trace is
// the same record as a full trace page and /tasks/<id>/trace.txt as plain
// text (taskTraceHandler).
func (s *Server) taskPageHandler(w http.ResponseWriter, r *http.Request) {
	id, rest, _ := strings.Cut(strings.Trim(strings.TrimPrefix(r.URL.Path, "/tasks/"), "/"), "/")

	if !tasks.ValidID(id) {
		http.NotFound(w, r)
		return
	}

	switch rest {
	case "":
	case "trace", "trace.txt":
		s.taskTraceHandler(w, r, id, rest == "trace.txt")
		return
	default:
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

	data := webui.TaskPageData{Task: detail}
	if s.Qualification != nil {
		if run, err := s.Qualification.ForTask(id); err == nil {
			data.ReadinessURL = "/readiness/" + run.Definition.WorkerID + "?evaluation=" + run.ID
		}
	}
	if err := s.Web.RenderTask(w, data); err != nil {
		internalError(w, "task page", err)
	}
}

// taskTraceHandler renders one task's full trace - /tasks/<id>/trace as a page
// in the site chrome, /tasks/<id>/trace.txt as plain text for sharing. Both
// read the task store only, so they are served on both listeners like the
// task page itself.
func (s *Server) taskTraceHandler(w http.ResponseWriter, r *http.Request, id string, asText bool) {
	if !allowMethod(w, r, http.MethodGet) {
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

	w.Header().Set("Cache-Control", "no-store")

	if asText {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Query().Get("download") != "" {
			w.Header().Set("Content-Disposition", `attachment; filename="decurix-trace-`+id+`.txt"`)
		}
		_, _ = io.WriteString(w, webui.TraceText(detail))
		return
	}

	if err := s.Web.RenderTaskTrace(w, webui.TaskTracePageData{Task: detail, RunRecords: !isGated(r) && s.RunStore != nil}); err != nil {
		log.Printf("task trace: render: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// taskOfWorker reports whether task is one of worker's tasks in the records: what a desktop may read scoped to a task.
func (s *Server) taskOfWorker(worker, task string) bool {
	if s.Tasks == nil || !tasks.ValidID(task) {
		return false
	}
	d, err := s.Tasks.Get(task)
	return err == nil && d.WorkerID == desktop.SanitizeID(worker)
}

// maxTaskPicks bounds how many of a desktop's tasks its card's dropdown lists.
const maxTaskPicks = 50

// maxTaskPickIDs bounds one /api/desktops/tasks call, like the activity poll.
const maxTaskPickIDs = 200

// TaskPick is one entry of a dashboard card's task dropdown. StatusLabel is the derived outcome's label
// (tasks.DisplayOutcome), so a done nobody's check judged reads "Done, not checked" there too.
type TaskPick struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Request     string        `json:"request"`
	Status      string        `json:"status"`
	StatusLabel string        `json:"status_label"`
	Checked     *bool         `json:"checked"`
	Outcome     tasks.Display `json:"outcome"`
	CreatedAt   time.Time     `json:"created_at"`
}

// desktopTasksHandler answers GET /api/desktops/tasks?ids=a,b,c with each
// named desktop's tasks newest first, keyed by ID - what the dashboard
// cards' task dropdowns poll, one request for every card.
func (s *Server) desktopTasksHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	out := map[string][]TaskPick{}

	for i, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if i >= maxTaskPickIDs {
			break
		}

		id := desktop.SanitizeID(strings.TrimSpace(raw))
		if id == "" {
			continue
		}

		list, err := s.Tasks.RecentForWorker(id, maxTaskPicks)
		if err != nil {
			jsonError(w, err)
			return
		}

		picks := make([]TaskPick, 0, len(list))
		for _, t := range list {
			picks = append(picks, TaskPick{
				ID:          t.ID,
				Title:       tasks.ShortTitle(t.Request, webui.ShortTitleRunes),
				Request:     t.Request,
				Status:      t.Status,
				StatusLabel: t.Outcome.Label,
				Checked:     t.Checked,
				Outcome:     t.Outcome,
				CreatedAt:   t.CreatedAt,
			})
		}
		out[id] = picks
	}

	jsonResponse(w, out)
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
	if !requirePost(w, r) {
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

	// A verdict is a person's judgement (plan R5): it is stored with who gave it, and a request with nobody to author
	// it (a desktop's own) is refused.
	actor, ok := s.actorOf(r)
	if !ok {
		jsonStatusError(w, http.StatusForbidden, "a verdict is a person's to give")
		return
	}
	task, err := s.Tasks.SetVerdict(id, verdict, note, actor)
	if errors.Is(err, tasks.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such task")
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	if task.Verdict == tasks.VerdictWrong && s.Qualification != nil {
		if err := s.Qualification.WrongTask(task.ID); err != nil {
			jsonError(w, err)
			return
		}
	}
	s.Chat.SetVerdict(task.WorkerID, task.ID, task.Verdict)

	jsonResponse(w, task)
}
