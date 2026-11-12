package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// maxQueryTextRequestBytes bounds one POST /api/querytext body. Content
// itself is capped at 16384 bytes below; this leaves headroom for the
// other fields and stops the full body being buffered before that check
// ever runs.
const maxQueryTextRequestBytes = 32768

// noteIDs hands out query note IDs: the time in nanoseconds, moved on by one
// when the clock has not moved since the last one. The time alone was the ID,
// and on Windows - where the clock a program reads ticks far more coarsely
// than a nanosecond - two notes arriving together got the same one, and the
// second was written over the first (querynotes.Store.Add updates on a
// conflict). TestReplyResolvesTheQueryNote failed on Windows for that reason.
var noteIDs struct {
	mu   sync.Mutex
	last int64
}

// nextNoteID returns an ID no other note of this process has.
func nextNoteID() string {
	noteIDs.mu.Lock()
	defer noteIDs.mu.Unlock()

	id := time.Now().UnixNano()
	if id <= noteIDs.last {
		id = noteIDs.last + 1
	}
	noteIDs.last = id

	return strconv.FormatInt(id, 10)
}

// agentWaitingHandler lists the tasks of a desktop that wait on a person (GET ?desktop_id=), for its agent coming
// back from a restart: each question it no longer holds it reports as interrupted (querytext, interrupted), which
// closes the task as stopped. A desktop sees only its own (actingAs).
func (s *Server) agentWaitingHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	acting, ok := actingAs(w, r, r.URL.Query().Get("desktop_id"))
	if !ok {
		return
	}
	id := desktop.SanitizeID(acting)
	if id == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}
	waiting, err := s.Tasks.Waiting(id)
	if err != nil {
		log.Printf("waiting tasks of %s: %v", id, err)
		jsonStatusError(w, http.StatusInternalServerError, "The tasks could not be read.")
		return
	}
	if waiting == nil {
		waiting = []string{}
	}
	jsonResponse(w, map[string][]string{"tasks": waiting})
}

// queryTextRequest is a desktop's message: a question, an approval, or how a turn ended.
type queryTextRequest struct {
	DesktopID string `json:"desktop_id"`
	VMName    string `json:"vm_name"`
	Filename  string `json:"filename"`
	Content   string `json:"content"`

	// Kind and Reply are the chat's own view of this callback: what sort
	// of entry it is, and just the part a human wants to read, without
	// the formatted note's headings. Sent by desktops built from a
	// current image; absent from older ones, which the handler falls
	// back to parsing out of Content.
	Kind  string `json:"kind"`
	Reply string `json:"reply"`

	// Quiet asks for no Query rail entry: the callback still closes the
	// task and answers in the chat. Sent for a result nobody needs paging
	// about, such as a finished recording, which the desktop card lists.
	Quiet bool `json:"quiet"`

	// Interrupted marks the report a restarted agent makes about the task
	// its previous process never finished (the desktop's X session ended
	// and the container came back on its own). It is recorded only while
	// that task is still open: a restart through the API closed it already
	// and said so.
	Interrupted bool `json:"interrupted"`

	// StillWorking says the desktop is still busy after this reply: an answer to a message a planned run took
	// mid-run (its receipt) is a reply in the chat, but the run goes on, so the turn stays open (running).
	StillWorking bool `json:"still_working"`

	// Checked says whether the desktop's final check judged a finished
	// result (messaging.write_final_reply). Absent from older images,
	// which leaves it unknown rather than "not checked".
	Checked *bool `json:"checked"`

	// Verdict is the final check, structured (decurix.verdict/v1, C5): stored beside the task, with checked
	// derived from it. Absent from older images.
	Verdict json.RawMessage `json:"verdict"`

	// Skill names the taught skill that drove the turn, and its version (C5): stored on the execution (R7).
	Skill *tasks.Skill `json:"skill"`

	// Secret marks a question raised for a secret (a password or code field): the person's reply to it is kept
	// withheld whole in every record, and still reaches the desktop as typed (X-6). Ignored on anything but a
	// question or an approval.
	Secret bool `json:"secret"`

	// What the task records need from the end of a turn: which task it
	// was, and what the desktop knows about the model and the spend so
	// far. All optional, and absent from older images.
	TaskID       string   `json:"task_id"`
	Engine       string   `json:"engine"`
	Model        string   `json:"model"`
	CostUSD      *float64 `json:"cost_usd"`
	InputTokens  *int64   `json:"input_tokens"`
	OutputTokens *int64   `json:"output_tokens"`
	// CachedInputTokens is the part of InputTokens the provider served
	// from its prompt cache.
	CachedInputTokens *int64 `json:"cached_input_tokens"`

	// Gate is the approval gate's own question for the action it holds, signed by the gate (an approval only).
	Gate *struct {
		ID       string `json:"id"`
		Question string `json:"question"`
		Sig      string `json:"sig"`
	} `json:"gate"`

	// EventID is the message's own id, the same on every resend (agent messaging.send_query): applied once.
	// Absent from older images, whose messages are always applied.
	EventID string `json:"event_id"`
}

// queryTextResult is the answer queryTextHandler gives: a JSON body, or an error status with its reason.
type queryTextResult struct {
	status int
	reason string
	body   any
}

// write sends the answer.
func (r queryTextResult) write(w http.ResponseWriter) {
	if r.status != 0 && r.status != http.StatusOK {
		jsonStatusError(w, r.status, r.reason)
		return
	}
	jsonResponse(w, r.body)
}

// queryTextHandler records a query note submitted from inside a desktop.
func (s *Server) queryTextHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	var req queryTextRequest

	r.Body = http.MaxBytesReader(w, r.Body, maxQueryTextRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badJSON(w, err)
		return
	}

	acting, ok := actingAs(w, r, req.DesktopID)
	if !ok {
		return
	}
	req.DesktopID = acting
	if req.DesktopID == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}
	// Qualification outcomes are produced by the host verifier, never agent
	// callbacks. An injected request cannot close or certify an exam task.
	desktopID := desktop.SanitizeID(req.DesktopID)
	if strings.HasPrefix(desktopID, "readiness-") || strings.HasPrefix(desktopID, "practice-") {
		jsonStatusError(w, http.StatusForbidden, "Readiness results can only be recorded by the independent runner.")
		return
	}

	if len(req.Content) > 16384 {
		http.Error(w, "content exceeds maximum size of 16384 bytes", http.StatusBadRequest)
		return
	}

	// A desktop resends a message until it is acknowledged (agent messaging.resend_unsent); one event id is applied
	// once, and acknowledged only after it is applied (internal/delivery).
	var out queryTextResult
	// A refused message (its task is not open) is not remembered as applied: it was not.
	if s.applyOnce(desktopID, req.EventID, func() bool {
		out = s.applyQueryText(desktopID, &req)
		return out.status == 0 || out.status == http.StatusOK
	}) {
		jsonResponse(w, map[string]any{"status": "received", "duplicate": true})
		return
	}
	out.write(w)
}

// applyQueryText applies a desktop's message to the task records, the Queries rail and the chat, and returns the
// answer to give.
func (s *Server) applyQueryText(desktopID string, req *queryTextRequest) queryTextResult {
	note := querynotes.QueryNote{
		ID:        nextNoteID(),
		DesktopID: req.DesktopID,
		VMName:    req.VMName,
		Filename:  req.Filename,
		Content:   req.Content,
		CreatedAt: time.Now(),
	}

	// Chat receives every reply. The Query rail keeps formatted notes only
	// for questions, approvals, and failures that need attention.
	chatText, notedKind := agentReplyFromNote(req.Content)

	if trimmed := strings.TrimSpace(req.Reply); trimmed != "" {
		chatText = trimmed
	}

	chatKind := chatKindFor(req.Kind)
	if chatKind == "" {
		chatKind = notedKind
	}

	if req.Interrupted {
		open, err := s.Tasks.IsOpen(desktopID, reportedTaskID(req.TaskID))
		if err != nil {
			log.Printf("querytext from %s: %v", desktopID, err)
		}
		// A task open only as an unconfirmed send (a restart cut its delivery short) is this report's to close.
		if !open && !s.acceptUnconfirmed(desktopID, reportedTaskID(req.TaskID)) {
			return queryTextResult{body: map[string]string{"status": "already closed"}}
		}
	}

	// The kind is what lets a later reply resolve this note: only a query or
	// an approval is answered by one.
	note.Kind = outcomeKindFor(req.Kind, chatKind)

	// An approval the gate worded: the person is shown the gate's words first, and their reply is bound to that
	// request. The worker's own text follows, said to be the worker's.
	if note.Kind == querynotes.KindApproval && req.Gate != nil &&
		s.Desktops.VerifyGateQuestion(desktopID, req.Gate.ID, req.Gate.Question, req.Gate.Sig) {
		question := strings.Join(strings.Fields(req.Gate.Question), " ")
		note.GateRequest, note.GateQuestion = req.Gate.ID, question
		note.Content = withGateQuestion(question, note.Content)
		chatText = withGateQuestion(question, chatText)
	}

	named := reportedTaskID(req.TaskID)
	verdict, problems := tasks.ParseVerdict(req.Verdict)
	if len(problems) > 0 {
		log.Printf("querytext from %s: the verdict for task %s (stored as %s): %s",
			desktopID, named, verdict.Verdict, strings.Join(problems, "; "))
	}
	outcome := tasks.Outcome{
		Verdict:           verdict,
		Skill:             req.Skill,
		Kind:              note.Kind,
		Reply:             chatText,
		Engine:            clip(req.Engine, 32),
		Model:             clip(req.Model, 128),
		CostUSD:           nonNegative(req.CostUSD),
		InputTokens:       nonNegative(req.InputTokens),
		OutputTokens:      nonNegative(req.OutputTokens),
		CachedInputTokens: cachedWithin(nonNegative(req.CachedInputTokens), nonNegative(req.InputTokens)),
		Checked:           req.Checked,
		Secret:            req.Secret && (note.Kind == tasks.OutcomeQuery || note.Kind == tasks.OutcomeApproval),
		Interrupted:       req.Interrupted,
	}
	taskID, execID, err := s.Tasks.RecordOutcomeExec(desktopID, named, outcome)
	// A task its desktop answers that the records never got (a restart cut its delivery short, and the desktop could
	// not be asked at the next start) is recorded now, then its outcome.
	if err == nil && named != "" && taskID == "" && s.acceptUnconfirmed(desktopID, named) {
		taskID, execID, err = s.Tasks.RecordOutcomeExec(desktopID, named, outcome)
	}
	if err != nil {
		log.Printf("querytext from %s: %v", desktopID, err)
	}

	// An ended execution records the deployment that served it (R7), from the call log (one query, no rows open).
	if err == nil && execID != "" && (note.Kind == tasks.OutcomeDone || note.Kind == tasks.OutcomeError) {
		s.resolveModel(execID)
		s.taskEnded(desktopID, taskID)
		// Its bundle leaves the desktop now (R9), before anything on the desktop can prune it.
		s.pullAfterOutcome(note.Kind, desktopID, taskID, execID)
	}

	// An outcome for a task that is not open here (it ended, or is not this desktop's) is refused, with nothing
	// shown: it must not close another task, and a resent copy must not land in the chat again. A 4xx is the
	// desktop's sign to let it go (messaging.resend_unsent drops a refused outcome) - review 2026-11-03, C-01/L-03.
	if err == nil && named != "" && taskID == "" {
		log.Printf("querytext from %s: task %s is not open, so its %s outcome was refused", desktopID, named, note.Kind)
		return queryTextResult{status: http.StatusConflict,
			reason: "That task is not open on this desktop, so this outcome was not recorded."}
	}

	if !req.Quiet && note.Kind != tasks.OutcomeDone {
		s.QueryNotes.Add(note)
	}

	if chatText != "" {
		if req.StillWorking {
			s.Chat.AppendAgentNote(desktopID, taskID, chatKind, chatText)
		} else {
			s.Chat.AppendAgentReply(
				desktopID,
				taskID,
				chatKind,
				chatText,
			)
		}
	}

	if note.Kind == tasks.OutcomeDone {
		s.recordFinished(desktopID, req.VMName, taskID, chatText)
	}

	// (A reply while the desktop is still working - a planned run's receipt - never speeds up its idle pause.)
	if s.IdleChecker != nil && !req.StillWorking && note.Kind == tasks.OutcomeDone {
		s.IdleChecker.FastPauseAfter(desktopID, 15*time.Second)
	}

	return queryTextResult{body: map[string]string{"status": "received"}}
}

// outcomeKindFor maps the kind a desktop reports on its callback to the task
// records' outcome. An older image reports no kind, so the chat kind read off
// its note decides - where an approval and a plain query look alike.
func outcomeKindFor(reported, chatKind string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case "approval":
		return tasks.OutcomeApproval
	case "query":
		return tasks.OutcomeQuery
	case "error":
		return tasks.OutcomeError
	case "final", "message":
		return tasks.OutcomeDone
	case "":
	default:
		// A kind this control plane does not know is not a success it can record: a person looks at it.
		return tasks.OutcomeQuery
	}

	switch chatKind {
	case conversation.KindApproval, conversation.KindQuery:
		return tasks.OutcomeQuery
	case conversation.KindError:
		return tasks.OutcomeError
	default:
		return tasks.OutcomeDone
	}
}

// nonNegative is value, or nil when it is negative.
func nonNegative[T float64 | int64](value *T) *T {
	if value == nil || *value < 0 {
		return nil
	}

	return value
}

// queryNoteView is a note as the rail and the Queries page show it: the
// stored note, plus the two parts of it a person reads - the task it is about
// and what the agent said. The note itself opens with "DECURIX QUERY REQUEST /
// Desktop: / Desktop ID:" boilerplate, which is all a card showed of it.
type queryNoteView struct {
	querynotes.QueryNote
	Task    string `json:"task,omitempty"`
	Message string `json:"message,omitempty"`
	// Gated says the desktop's approval gate worded this approval: a button bound to it (note_id) can grant it.
	Gated bool `json:"gated,omitempty"`
}

// viewQueryNotes adds each note's task and message, read out of its text so
// notes stored before either was sent separately read the same way.
func viewQueryNotes(notes []querynotes.QueryNote) []queryNoteView {
	views := make([]queryNoteView, 0, len(notes))
	for _, note := range notes {
		views = append(views, queryNoteView{QueryNote: note, Task: note.Task(), Message: note.Message(), Gated: note.GateRequest != ""})
	}
	return views
}

// queryNotesHandler returns the query notes still open as JSON.
func (s *Server) queryNotesHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, viewQueryNotes(s.QueryNotes.List()))
}

// resolvedQueryNotesHandler returns the resolved query notes as JSON, most
// recently resolved first - the Queries page's history.
func (s *Server) resolvedQueryNotesHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, viewQueryNotes(s.QueryNotes.ListResolved()))
}

// closeQueryNoteHandler resolves a query note by ID; it stays in the history.
func (s *Server) closeQueryNoteHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	id := r.FormValue("id")

	s.QueryNotes.Close(id)
	// The card's needs-you rests on the open notes: every page fetches its cards again (events.go).
	s.events.publish("")

	jsonResponse(w, map[string]string{
		"status": "closed",
	})
}

// dashboardPostsHandler renders the query-notes dashboard, most recent first.
func (s *Server) dashboardPostsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}

	notes := s.QueryNotes.List()

	sort.Slice(notes, func(i, j int) bool {
		return notes[i].CreatedAt.After(notes[j].CreatedAt)
	})

	data := webui.QueryNotesPageData{
		Notes: notes,
	}

	if err := s.Web.RenderQueryNotesDashboard(w, data); err != nil {
		internalError(w, "queries dashboard", err)
		return
	}
}

// cachedWithin is the cached share of the input, never more than the input it
// is part of; nil when either is unknown.
func cachedWithin(cached, input *int64) *int64 {
	if cached == nil || input == nil {
		return nil
	}
	if *cached > *input {
		v := *input
		return &v
	}
	return cached
}

// withGateQuestion is an approval's text with the gate's own question first and the worker's words after it, marked as
// the worker's (or the gate's words alone when the worker's only repeat them).
func withGateQuestion(question, worker string) string {
	worker = strings.TrimSpace(worker)
	if worker == "" || strings.Join(strings.Fields(worker), " ") == question {
		return question
	}
	return question + "\n\nThe worker's note: " + worker
}
