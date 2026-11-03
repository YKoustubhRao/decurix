// The soul - TheBot's main process. Wires config, workspace, the Ollama
// brain, the atomizer, the scheduler, and every registered skill together
// behind a loopback-only HTTP+SSE API - "embedded, not an external API"
// means this is how the VM's own local UI talks to its own local
// backend, never a network-facing contract for third parties.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"thebot/internal/atomizer"
	"thebot/internal/config"
	"thebot/internal/feedback"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/migrate"
	"thebot/internal/rag"
	"thebot/internal/scheduler"
	"thebot/internal/session"
	"thebot/internal/skills"
	"thebot/internal/trace"
	"thebot/internal/withheld"
	"thebot/internal/workspace"
	"thebot/web"
)

type jobRecord struct {
	mu            sync.Mutex
	ID            string
	Prompt        string
	Status        string
	Plan          []skills.Subtask
	Results       []skills.Result
	FinalAnswer   string
	UsedSkill     string
	ProposedSkill string
	// Outcome, Question, Route and Steps are the scheduler's structured
	// account of the job - what a host needs to act on it without parsing
	// FinalAnswer (see scheduler.Outcome).
	Outcome  scheduler.Outcome
	Question string
	// Secret is set when the answer to Question is a secret (scheduler.Job.Secret): "secret" in the job's JSON.
	Secret  bool
	Route   string
	Steps   []scheduler.StepRecord
	Resumed bool
	Log     []string
	// Usage is what this job's model calls read and wrote (llm.Metered).
	Usage *llm.Usage
	// secrets are the secret values withheld from everything this job logs and answers (C14).
	secrets withheld.Values
	// TaskID is the control plane's task the job was sent for ("" when none was named).
	TaskID      string
	startedAt   time.Time
	cancel      context.CancelFunc
	done        chan struct{}
	subscribers map[chan string]bool
}

func newJobRecord(id, prompt string) *jobRecord {
	return &jobRecord{ID: id, Prompt: prompt, Status: "running", startedAt: time.Now(), done: make(chan struct{}), subscribers: map[chan string]bool{}}
}

const (
	// maxRunningJobs caps how many jobs run at once. Each job is a model-driven loop that holds memory and a worker
	// shell, and the worker (which reads untrusted email and web pages) drives soul's API; without a cap, a flood of
	// /api/chat posts spawns unbounded goroutines and job records. Over the cap a new request is refused (429) rather
	// than queued, so the caller backs off instead of soul growing without bound. The scheduler's own model-
	// concurrency limit (llm.Limited) still bounds the model calls these running jobs make.
	maxRunningJobs = 16
	// maxJobRecords bounds how many finished job records are kept for status reads; the oldest finished ones are
	// pruned past it, so the jobs map does not grow for the life of the process.
	maxJobRecords = 256
)

// finish marks the job complete - a real channel close, not a magic
// string threaded through the log line stream (that was tried first and
// found broken live: a client connecting AFTER completion would replay
// the sentinel as an ordinary log line instead of getting the final
// event, since backlog replay didn't special-case it).
func (j *jobRecord) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	select {
	case <-j.done:
	default:
		close(j.done)
	}
}

func (j *jobRecord) isDone() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// appendLog adds a line to the job's log and stream, with the job's secret values withheld: a step's text
// ("$ gui type e3 '...'") leaves soul without them (C14).
func (j *jobRecord) appendLog(line string) {
	j.mu.Lock()
	line = j.secrets.Text(line)
	j.Log = append(j.Log, line)
	subs := make([]chan string, 0, len(j.subscribers))
	for ch := range j.subscribers {
		subs = append(subs, ch)
	}
	j.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- line:
		default:
		}
	}
}

func (j *jobRecord) subscribe() (chan string, []string) {
	ch := make(chan string, 128)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.subscribers[ch] = true
	backlog := append([]string{}, j.Log...)
	return ch, backlog
}

func (j *jobRecord) unsubscribe(ch chan string) {
	j.mu.Lock()
	delete(j.subscribers, ch)
	j.mu.Unlock()
}

// snapshot is the job as /api/jobs and the stream answer it, with the job's secret values withheld in every string.
func (j *jobRecord) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	promptTokens, responseTokens, calls := j.Usage.Snapshot()
	return scrubbed(map[string]any{
		"id": j.ID, "prompt": j.Prompt, "status": j.Status,
		"plan": j.Plan, "results": j.Results, "finalAnswer": j.FinalAnswer,
		"usedSkill": j.UsedSkill, "proposedSkill": j.ProposedSkill,
		"outcome": j.Outcome, "question": j.Question, "secret": j.Secret, "route": j.Route,
		"steps": j.Steps, "resumed": j.Resumed,
		"usage": map[string]int{"promptTokens": promptTokens, "responseTokens": responseTokens, "calls": calls,
			"cachedTokens": j.Usage.Cached()},
	}, j.secrets.Text)
}

// scrubbed is v as its JSON reads, with fix applied to every string in it (keys too): what soul answers is filtered
// once, whatever shape it has.
func scrubbed(v map[string]any, fix func(string) string) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	out, _ := scrubAny(decoded, fix).(map[string]any)
	return out
}

func scrubAny(x any, fix func(string) string) any {
	switch t := x.(type) {
	case string:
		return fix(t)
	case []any:
		for i := range t {
			t[i] = scrubAny(t[i], fix)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fix(k)] = scrubAny(val, fix)
		}
		return out
	}
	return x
}

// secretSet is every secret value soul has been told this process, at most maxSecrets, oldest dropped first. It is
// never written anywhere: it lets a job resumed in a session, and the memory API, withhold values an earlier job was
// told.
type secretSet struct {
	mu   sync.Mutex
	vals []string
}

const maxSecrets = 256

// add keeps values and returns every value known.
func (s *secretSet) add(values []string) withheld.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range withheld.Clean(values) {
		if !slices.Contains(s.vals, v) {
			s.vals = append(s.vals, v)
		}
	}
	if len(s.vals) > maxSecrets {
		s.vals = s.vals[len(s.vals)-maxSecrets:]
	}
	return withheld.Clean(s.vals)
}

// all is every value known.
func (s *secretSet) all() withheld.Values { return s.add(nil) }

// server holds everything the HTTP handlers need - deliberately a struct,
// not package-level globals, so tests can construct isolated instances
// instead of sharing mutable state across the whole test binary.
type server struct {
	cfg      config.Config
	provider llm.Provider
	ws       *workspace.Workspace
	sched    *scheduler.Scheduler
	sessions *session.Store

	jobsMu sync.Mutex
	jobs   map[string]*jobRecord

	// paused holds each session's paused job, if it has one. A session has
	// at most one: the next request in it either resumes that job or, when
	// the host says it is not a reply, discards it.
	pausedMu sync.Mutex
	paused   map[string]*scheduler.Checkpoint
	// storesOff are the stores this run started without (storeOff), shown by /api/health.
	storesOff []string
	// tracer writes each task's trace (internal/trace); nil writes none.
	tracer *trace.Writer
	// secrets is every secret value a job was told this process (C14), withheld from all soul answers.
	secrets secretSet
}

// pausedJobTTL is how long a paused job waits for its reply before the next
// request in the session is treated as a new request.
const pausedJobTTL = 24 * time.Hour

func newServer(cfg config.Config, provider llm.Provider, ws *workspace.Workspace, sched *scheduler.Scheduler) *server {
	return &server{cfg: cfg, provider: provider, ws: ws, sched: sched, sessions: session.NewStore(), jobs: map[string]*jobRecord{}, paused: map[string]*scheduler.Checkpoint{}}
}

// takePaused removes and returns a session's paused job, if it has a live one.
func (s *server) takePaused(sessionID string) *scheduler.Checkpoint {
	if sessionID == "" {
		return nil
	}
	s.pausedMu.Lock()
	defer s.pausedMu.Unlock()
	cp := s.paused[sessionID]
	delete(s.paused, sessionID)
	if cp == nil || time.Since(cp.CreatedAt) > pausedJobTTL {
		return nil
	}
	return cp
}

func (s *server) storePaused(sessionID string, cp *scheduler.Checkpoint) {
	if sessionID == "" || cp == nil {
		return
	}
	s.pausedMu.Lock()
	// Paused jobs past their wait are dropped as new ones come: a soul that lives for months does not keep every
	// session's last question (review T-24).
	for id, old := range s.paused {
		if time.Since(old.CreatedAt) > pausedJobTTL {
			delete(s.paused, id)
		}
	}
	s.paused[sessionID] = cp
	s.pausedMu.Unlock()
}

func genID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	writeJSON(w, 200, map[string]any{"ollama": s.provider.IsReachable(ctx), "ollamaHost": s.cfg.OllamaHost, "workspace": s.ws.Root(),
		"storesOff": s.storesOff})
}

// handleRAGStats and handleFeedbackStats back the UI's live header
// counters with real numbers straight from the actual stores - "no
// invisibility" applies to the UI just as much as it did to file paths.
func (s *server) handleRAGStats(w http.ResponseWriter, r *http.Request) {
	if s.sched.RAG == nil {
		writeJSON(w, 200, map[string]any{"totalChunks": 0, "totalFiles": 0})
		return
	}
	chunks, files, err := s.sched.RAG.Stats(r.Context())
	if err != nil {
		log.Printf("[soul] %s: %v", r.URL.Path, err)
		writeJSON(w, 500, map[string]any{"error": "could not read the index"})
		return
	}
	writeJSON(w, 200, map[string]any{"totalChunks": chunks, "totalFiles": files})
}

func (s *server) handleFeedbackStats(w http.ResponseWriter, r *http.Request) {
	if s.sched.Feedback == nil {
		writeJSON(w, 200, map[string]any{"negativeCount": 0})
		return
	}
	n, err := s.sched.Feedback.Count(r.Context())
	if err != nil {
		log.Printf("[soul] %s: %v", r.URL.Path, err)
		writeJSON(w, 500, map[string]any{"error": "could not read the feedback store"})
		return
	}
	writeJSON(w, 200, map[string]any{"negativeCount": n})
}

// handleMemory is the single, honest answer to "what does the agent
// actually remember" - every memory tier that exists today (L1's curated
// cross-session facts, L2's raw job log, the Data RAG's real indexed
// files, the feedback store's real entries), aggregated with real data,
// not just counts: the actual facts, the actual file list, the actual
// recent entries, so nothing stored is hidden behind a number alone.
//
// L1 was missing here until live testing caught it: recall was demonstrably
// working ("recalled 3 fact(s) from L1" in the logs, and the right answer
// coming back in a brand-new session) while this endpoint reported no L1 at
// all, so the drawer showed nothing. The tier the user most wants to see -
// what it remembers ABOUT THEM - was the one tier not surfaced.
func (s *server) handleMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{}

	l1Info := map[string]any{"totalFacts": 0, "recent": []any{}}
	if s.sched.L1 != nil {
		if n, err := s.sched.L1.Count(ctx); err == nil {
			l1Info["totalFacts"] = n
		}
		if recent, err := s.sched.L1.ListRecent(ctx, 20); err == nil {
			facts := make([]any, 0, len(recent))
			for _, e := range recent {
				facts = append(facts, map[string]any{
					"fact":      e.Fact,
					"source":    e.Source,
					"createdAt": time.Unix(e.CreatedAt, 0).UTC().Format(time.RFC3339),
				})
			}
			l1Info["recent"] = facts
		}
	}
	out["l1"] = l1Info

	l2Info := map[string]any{"totalJobs": 0, "recent": []any{}}
	if s.sched.L2 != nil {
		if n, err := s.sched.L2.Count(); err == nil {
			l2Info["totalJobs"] = n
		}
		if recent, err := s.sched.L2.Recent(10); err == nil {
			l2Info["recent"] = recent
		}
	}
	out["l2"] = l2Info

	ragInfo := map[string]any{"totalChunks": 0, "totalFiles": 0, "files": []any{}}
	if s.sched.RAG != nil {
		if chunks, files, err := s.sched.RAG.Stats(ctx); err == nil {
			ragInfo["totalChunks"] = chunks
			ragInfo["totalFiles"] = files
		}
		if list, err := s.sched.RAG.ListFiles(ctx); err == nil {
			ragInfo["files"] = list
		}
	}
	out["rag"] = ragInfo

	fbInfo := map[string]any{"totalEntries": 0, "recent": []any{}}
	if s.sched.Feedback != nil {
		if n, err := s.sched.Feedback.Count(ctx); err == nil {
			fbInfo["totalEntries"] = n
		}
		if recent, err := s.sched.Feedback.ListRecent(ctx, 10); err == nil {
			fbInfo["recent"] = recent
		}
	}
	out["feedback"] = fbInfo

	// A second wall (C14): what the stores hold passes through the secret values soul was told and its secrets
	// filter before it is served, whatever was written before either existed.
	secrets := s.secrets.all()
	writeJSON(w, 200, scrubbed(out, func(text string) string { return memory.RedactSecrets(secrets.Text(text)) }))
}

// handleSkills lists real acquired-skill state: what's confirmed and
// actually in use, and what's pending a real human decision. Nothing
// here is ever silently trusted - a pending skill is a genuine proposal
// sitting on disk, matched against nothing until acted on.
func (s *server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if s.sched.SkillLib == nil {
		writeJSON(w, 200, map[string]any{"confirmed": []any{}, "pending": []any{}})
		return
	}
	confirmed, err := s.sched.SkillLib.ListConfirmed()
	if err != nil {
		log.Printf("[soul] %s: %v", r.URL.Path, err)
		writeJSON(w, 500, map[string]any{"error": "could not list the skills"})
		return
	}
	pending, err := s.sched.SkillLib.ListPending()
	if err != nil {
		log.Printf("[soul] %s: %v", r.URL.Path, err)
		writeJSON(w, 500, map[string]any{"error": "could not list the skills"})
		return
	}
	writeJSON(w, 200, map[string]any{"confirmed": confirmed, "pending": pending})
}

// handleSkillAction is POST /api/skills/{slug}/confirm or .../reject -
// the one real point where a human's decision turns a proposal into
// something trusted, or discards it for good.
func (s *server) handleSkillAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sched.SkillLib == nil {
		// Off (inside Decurix, or with no embedder): there is nothing here to confirm.
		writeJSON(w, 404, map[string]any{"error": "no skill library on this machine"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/skills/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeJSON(w, 404, map[string]any{"error": "expected /api/skills/{slug}/confirm or /reject"})
		return
	}
	slug, action := parts[0], parts[1]

	switch action {
	case "confirm":
		skill, err := s.sched.SkillLib.Confirm(r.Context(), slug)
		if err != nil {
			log.Printf("[soul] %s: %v", r.URL.Path, err)
			writeJSON(w, 404, map[string]any{"error": "no such skill"})
			return
		}
		writeJSON(w, 200, map[string]any{"confirmed": skill})
	case "reject":
		if err := s.sched.SkillLib.Reject(slug); err != nil {
			log.Printf("[soul] %s: %v", r.URL.Path, err)
			writeJSON(w, 404, map[string]any{"error": "no such skill"})
			return
		}
		writeJSON(w, 200, map[string]any{"rejected": slug})
	default:
		writeJSON(w, 404, map[string]any{"error": "unknown action - use /confirm or /reject"})
	}
}

// maxChatBody bounds a chat request's body.
const maxChatBody = 4 << 20

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Prompt    string `json:"prompt"`
		SessionID string `json:"sessionId"`
		// Context is the host's standing context (who this machine is, its
		// worker memory, the rules it runs under). It reaches every model
		// call through the system frame and is never treated as the task.
		Context string `json:"context"`
		// WorkDir is where commands run and relative paths resolve.
		WorkDir string `json:"workdir"`
		// Reply says whether this request answers the session's paused
		// job. Unset means "if there is one"; false discards it.
		Reply *bool `json:"reply"`
		// No "approved" field: a request never approves itself. A yes reaches
		// a job only as the reply to its own paused approval (Reply, with
		// ApprovalReading), for that one action, once.
		// ApprovalReading is the host's one reading of a reply to an approval
		// (see scheduler.Request.Reading), or null.
		ApprovalReading *skills.Reading `json:"approvalReading"`
		// PlanAgreed is the host's own word that this request is a stage of a plan the person already agreed: its goal
		// puts no plan of its own to them. A flag in the request body, never a marker read from the request's text,
		// which can hold pasted content (review T-32).
		PlanAgreed bool `json:"planAgreed"`
		// TaskID is the control plane's task this request belongs to (PLAN-P2 C2). It tags every model call and command
		// of the job (llm.WithTags) and names its trace; one not of llm.TaskIDShape is ignored.
		TaskID string `json:"taskId"`
		// Secrets are the request's (and its replies') secret values: what the person marked, or the control plane
		// withheld (C14). Data beside the prompt, never added to it: the model is sent the prompt as it is, and
		// nothing soul logs, answers or stores keeps a copy of them.
		Secrets []string `json:"secrets"`
	}
	// Bounded: a request is a prompt and its host context, never megabytes (review T-23).
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, 400, map[string]any{"error": "prompt is required"})
		return
	}

	id := genID()
	job := newJobRecord(id, body.Prompt)
	job.secrets = s.secrets.add(body.Secrets)
	s.jobsMu.Lock()
	running := 0
	for _, j := range s.jobs {
		if !j.isDone() {
			running++
		}
	}
	if running >= maxRunningJobs {
		s.jobsMu.Unlock()
		writeJSON(w, 429, map[string]any{"error": fmt.Sprintf("busy: %d jobs already running (max %d) - retry shortly", running, maxRunningJobs)})
		return
	}
	s.pruneFinishedJobsLocked()
	s.jobs[id] = job
	s.jobsMu.Unlock()

	// Real conversation history for this session, threaded into the job
	// before it runs - every prior turn is actually handed to the model.
	history := s.sessions.History(body.SessionID)

	req := scheduler.Request{Prompt: body.Prompt, Context: body.Context, WorkDir: body.WorkDir, History: history,
		Reading: body.ApprovalReading, PlanAgreed: body.PlanAgreed}
	if paused := s.takePaused(body.SessionID); paused != nil {
		if body.Reply == nil || *body.Reply {
			req.Resume = paused
			job.Resumed = true
			job.appendLog(fmt.Sprintf("scheduler: this request answers the paused job %q", truncateLog(paused.Prompt, 80)))
		} else {
			job.appendLog(fmt.Sprintf("scheduler: dropping the paused job %q - this is a new request", truncateLog(paused.Prompt, 80)))
		}
	}

	taskID := body.TaskID
	if !llm.TaskIDShape.MatchString(taskID) {
		taskID = ""
	}
	tags := llm.Tags{Task: taskID, Job: id}
	job.TaskID = taskID

	jobCtx := withheld.With(trace.WithWriter(llm.WithTags(context.Background(), tags), s.tracer), job.secrets)
	ctx, cancel := context.WithCancel(jobCtx)
	ctx, used := llm.WithUsage(ctx)
	job.mu.Lock()
	job.cancel = cancel
	job.Usage = used
	job.mu.Unlock()

	go func() {
		defer cancel()
		// With an approval gate a reading in the request is not believed; the gate's settlement of the paused job's
		// own request is (gate.go). Waited for here, in the job, so the request that resumes it is answered at once.
		if req.Resume != nil && gatePresent() {
			if _, _, ok := req.Resume.Approval(); ok {
				req.Reading = gateSettled(req.Resume)
			} else {
				req.Reading = nil
			}
		}
		result, err := s.sched.Execute(ctx, req, job.appendLog)
		job.mu.Lock()
		if err != nil {
			job.Status = "error"
			job.Outcome = scheduler.OutcomeFailed
			job.FinalAnswer = err.Error()
		} else {
			job.Status = result.Status
			job.Plan = result.Plan
			job.Results = result.Results
			job.FinalAnswer = result.FinalAnswer
			job.UsedSkill = result.UsedSkill
			job.ProposedSkill = result.ProposedSkill
			job.Outcome = result.Outcome
			job.Question = result.Question
			job.Secret = result.Secret
			job.Route = string(result.Route)
			job.Steps = result.Steps
			if result.Prompt != "" {
				job.Prompt = result.Prompt
			}
		}
		finalAnswer := job.FinalAnswer
		job.mu.Unlock()

		if err == nil && result.Checkpoint != nil {
			// A paused approval becomes the gate's request before anyone can be asked about it (gate.go).
			if gatePresent() {
				gateAsk(result.Checkpoint)
			}
			s.storePaused(body.SessionID, result.Checkpoint)
		}

		// Record BOTH sides of the exchange so the next request in this
		// session sees it - the human's own words, not the host context.
		s.sessions.Append(body.SessionID, llm.Message{Role: "user", Content: body.Prompt})
		s.sessions.Append(body.SessionID, llm.Message{Role: "assistant", Content: finalAnswer})

		// L1: judge whether this exchange contained something durably worth
		// remembering. The judgment is a whole model call, so it is only
		// made when the human's words look like they carry something about
		// themselves - on a CPU-only host, an unconditional judge after
		// every "check the disk space" is a minute of the model's time the
		// next request queues behind, and it evicts that request's cached
		// prompt prefix on the way.
		if s.sched.L1 != nil && memory.LooksMemorable(body.Prompt) {
			go s.sched.L1.ConsiderExchange(jobCtx, body.Prompt, finalAnswer, job.appendLog)
		}

		job.finish()
	}()

	writeJSON(w, 200, map[string]any{"id": id})
}

// pruneFinishedJobsLocked drops the oldest finished job records once the map is at maxJobRecords, so a long-lived
// soul does not keep every job it ever ran. A running job is never pruned. Called with jobsMu held.
func (s *server) pruneFinishedJobsLocked() {
	if len(s.jobs) < maxJobRecords {
		return
	}
	var finished []*jobRecord
	for _, j := range s.jobs {
		if j.isDone() {
			finished = append(finished, j)
		}
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].startedAt.Before(finished[b].startedAt) })
	for i := 0; i < len(finished) && len(s.jobs) >= maxJobRecords; i++ {
		delete(s.jobs, finished[i].ID)
	}
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.Split(rest, "/")
	id := parts[0]

	s.jobsMu.Lock()
	job, ok := s.jobs[id]
	s.jobsMu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no such job"})
		return
	}

	if len(parts) == 2 && parts[1] == "stream" {
		s.streamJob(w, r, job)
		return
	}

	// POST /api/jobs/{id}/cancel stops a running job: its context is
	// cancelled, which kills the command it is running and abandons the
	// model call it is waiting on. The job still finishes normally, with
	// what it got done.
	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job.mu.Lock()
		cancel := job.cancel
		job.mu.Unlock()
		if cancel != nil && !job.isDone() {
			job.appendLog("scheduler: cancelled on request")
			cancel()
		}
		writeJSON(w, 200, map[string]any{"id": job.ID, "cancelled": true})
		return
	}

	writeJSON(w, 200, job.snapshot())
}

func (s *server) streamJob(w http.ResponseWriter, r *http.Request, job *jobRecord) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, backlog := job.subscribe()
	defer job.unsubscribe(ch)

	for _, line := range backlog {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "log", "line": line}))
	}
	flusher.Flush()

	// The job may already have finished before this client ever connected
	// (or between subscribing and reaching here) - checked explicitly
	// rather than relying on a magic value ever appearing on ch, which is
	// exactly the bug this replaced (found via live testing: a client
	// connecting after completion got nothing, since backlog replay never
	// special-cased the old "__DONE__" sentinel).
	if job.isDone() {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "final", "job": job.snapshot()}))
		flusher.Flush()
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-job.done:
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "final", "job": job.snapshot()}))
			flusher.Flush()
			return
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "log", "line": line}))
			flusher.Flush()
		}
	}
}

// mux is soul's routes behind its guard (guard.go).
func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/rag/stats", s.handleRAGStats)
	mux.HandleFunc("/api/feedback/stats", s.handleFeedbackStats)
	mux.HandleFunc("/api/memory", s.handleMemory)
	mux.HandleFunc("/api/skills", s.handleSkills)
	mux.HandleFunc("/api/skills/", s.handleSkillAction)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/jobs/", s.handleJobs)
	// The UI itself - embedded in the binary (see web/embed.go), served at
	// "/". This is the ONLY thing this loopback server exists to serve to
	// a browser; everything else is the JSON/SSE API the UI's own JS calls.
	mux.Handle("/", http.FileServer(http.FS(web.FS)))
	return s.guard(mux)
}

// modelConcurrency is how many chat calls may run at once. A local Ollama
// answers one request per model at a time unless it was started with
// OLLAMA_NUM_PARALLEL, so the default there is one; the hosted APIs take
// several.
func modelConcurrency(cfg config.Config) int {
	fallback := 4
	if cfg.Provider == "ollama" {
		fallback = llm.EnvInt("OLLAMA_NUM_PARALLEL", 1)
	}
	return llm.EnvInt("MODEL_CONCURRENCY", fallback)
}

// buildBrain works out what the chat model can do. A hosted brain is
// described by the control plane (it tested the model when the key was
// saved); a local one is asked of Ollama itself, in the background, since
// Ollama may come up after soul.
func buildBrain(cfg config.Config, model string) *llm.Brain {
	caps := llm.Capabilities{Brain: cfg.Brain, Vision: cfg.BrainVision == "1"}
	if cfg.Provider != "ollama" {
		caps.Hosted = true
	}
	if caps.Brain == "" {
		caps.Brain = cfg.Provider
	}
	brain := llm.NewBrain(caps)

	if cfg.Provider == "ollama" {
		go brain.LearnFromOllama(context.Background(), cfg.OllamaHost, model, cfg.BrainVision != "", log.Printf)
	} else {
		log.Printf("[soul] brain: %s (hosted) - vision %v", caps.Brain, caps.Vision)
	}
	return brain
}

// ollamaEmbedder is Ollama as the embedder alone, for a chat provider that
// cannot embed (why says which), or nil - RAG and feedback off for this run -
// when no Ollama is configured or reachable. A nil *OllamaProvider must not
// be returned as a non-nil Embedder, so the nil is spelt out.
func ollamaEmbedder(cfg config.Config, why string) llm.Embedder {
	if cfg.OllamaHost == "" {
		log.Printf("[soul] no OLLAMA_HOST configured for embeddings (%s) - Data RAG and feedback are disabled for this run", why)
		return nil
	}
	ollama := llm.NewOllama(cfg.OllamaHost)
	ollama.EmbedModel = cfg.EmbedModel
	if !ollama.IsReachable(context.Background()) {
		log.Printf("[soul] Ollama not reachable at %s - Data RAG and feedback are disabled for this run", cfg.OllamaHost)
		return nil
	}
	log.Printf("[soul] using Ollama at %s for embeddings only (%s)", cfg.OllamaHost, why)
	return ollama
}

// buildProvider selects the real chat brain and, separately, a real
// embedder for the Data RAG - LLM_PROVIDER governs chat; the embedder
// follows from it EXCEPT for Anthropic, which has no embeddings API at
// all, so that case falls back to Ollama (if configured and actually
// reachable) or disables RAG/feedback for this run rather than crashing.
func buildProvider(cfg config.Config) (llm.Provider, llm.Models, llm.Embedder) {
	switch cfg.Provider {
	case "openai":
		p := llm.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIEmbedModel)
		if cfg.OpenAIBaseURL != "" {
			p.BaseURL = strings.TrimRight(cfg.OpenAIBaseURL, "/")
		}
		models := llm.DefaultModels(cfg.OpenAIModel).WithCoding(cfg.CodingModel)
		if cfg.EmbedProvider == "ollama" {
			return p, models, ollamaEmbedder(cfg, "the chat provider's endpoint is not asked for embeddings")
		}
		return p, models, p // OpenAI does both chat and embed - one provider, no fallback needed
	case "anthropic":
		chat := llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel)
		return chat, llm.DefaultModels(cfg.AnthropicModel).WithCoding(cfg.CodingModel), ollamaEmbedder(cfg, "Anthropic has no embeddings API")
	default: // "ollama"
		p := llm.NewOllama(cfg.OllamaHost)
		p.EmbedModel = cfg.EmbedModel
		return p, llm.DefaultModels(cfg.ChatModel).WithCoding(cfg.CodingModel), p
	}
}

// storeOff reports whether a store's init error is only that a newer soul wrote its database: then soul starts
// without that store (it is memory or a cache, nil-safe everywhere), says so in the log, and lists it in off for
// /api/health. Any other error is not one to start through (review N-W3-7).
func storeOff(name string, err error, off *[]string) bool {
	if !errors.Is(err, migrate.ErrNewer) {
		return false
	}
	log.Printf("[soul] %s is OFF for this run: %v. Starting without it; a newer soul will open it again.", name, err)
	*off = append(*off, name)
	return true
}

// standaloneMode reports whether soul runs as a product of its own (THEBOT_STANDALONE=1): only then are the acquired
// skill library and the coding harness on (owner decision O-8).
func standaloneMode() bool {
	return os.Getenv("THEBOT_STANDALONE") == "1"
}

// skillLibraryFor is the acquired-skill library, or nil: off unless standalone, and off without an embedder.
func skillLibraryFor(standalone bool, root string, embedder llm.Embedder, model string) (*memory.SkillLibrary, error) {
	if !standalone || embedder == nil {
		return nil, nil
	}
	return memory.NewSkillLibrary(root, embedder, model)
}

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("[soul] config error: %v", err)
	}
	log.Printf("[soul] workspace root: %s", cfg.WorkspaceRoot)
	log.Printf("[soul] llm provider: %s", cfg.Provider)
	ws, err := workspace.New(cfg.WorkspaceRoot)
	if err != nil {
		log.Fatalf("[soul] workspace init failed: %v", err)
	}

	l2, err := memory.NewL2(ws.Root())
	if err != nil {
		log.Fatalf("[soul] L2 memory init failed: %v", err)
	}

	rawProvider, models, embedder := buildProvider(cfg)
	// Every chat call in the process - scheduler steps running side by
	// side, L1's judge, the coding harness - shares one concurrency limit.
	// Each call of a task leaves a line in that task's trace (trace.Calls,
	// inside Metered, which settles the call's purpose first).
	provider := llm.Limited(llm.Metered(trace.Calls(rawProvider)), modelConcurrency(cfg))
	log.Printf("[soul] model concurrency: %d", modelConcurrency(cfg))

	// Which model does which job, said out loud at startup. A hybrid setup
	// that silently routes everything to one model looks exactly like a
	// working one until the quality difference shows up in the output.
	if models.Coding != "" && models.Coding != models.Default {
		log.Printf("[soul] models: %s for chat/planning, %s for coding", models.Default, models.Coding)
	} else {
		log.Printf("[soul] models: %s for everything (set CODING_MODEL to route builds to a dedicated coder)", models.Default)
	}

	var dataRAG *rag.RAG
	var fb *feedback.Store
	var storesOff []string // stores not opened because a newer soul wrote them (storeOff)
	if embedder != nil {
		dataRAG, err = rag.New(ws.Root(), embedder, cfg.DoclingScript)
		if err != nil {
			if !storeOff("the Data RAG", err, &storesOff) {
				log.Fatalf("[soul] Data RAG init failed: %v", err)
			}
			dataRAG = nil
		} else {
			defer dataRAG.Close()
		}

		fb, err = feedback.New(ws.Root(), embedder)
		if err != nil {
			if !storeOff("the feedback store", err, &storesOff) {
				log.Fatalf("[soul] feedback store init failed: %v", err)
			}
			fb = nil
		} else {
			defer fb.Close()
		}
	}

	// L1: curated cross-session memory. Needs both a brain (to judge what
	// is worth keeping) and an embedder (to recall it later), so it only
	// exists when an embedder does - same nil-safe pattern as RAG.
	var l1 *memory.L1
	if embedder != nil {
		l1, err = memory.NewL1(ws.Root(), provider, embedder, models.Judgment)
		if err != nil {
			if !storeOff("long-term memory (L1)", err, &storesOff) {
				log.Fatalf("[soul] L1 memory init failed: %v", err)
			}
			l1 = nil
		} else {
			defer l1.Close()
		}
	}

	// The parts left from soul's life as a product of its own - acquired skills, remembered build fixes and the coding
	// harness - are off unless THEBOT_STANDALONE=1 (owner decision O-8; review T-07, T-08, T-21). Inside Decurix a
	// skill must never be confirmed by anything the worker's shell can reach, and Decurix has its own taught skills.
	standalone := standaloneMode()
	log.Printf("[soul] standalone parts (acquired skills, coding harness): %v (THEBOT_STANDALONE=1 turns them on; "+
		"approval gate present: %v)", standalone, gatePresent())

	// SkillLib: acquired procedural memory. Same nil-safe pattern - needs
	// an embedder to match a future request against what's been learned.
	skillLib, err := skillLibraryFor(standalone, ws.Root(), embedder, models.Judgment)
	if err != nil {
		log.Fatalf("[soul] skill library init failed: %v", err)
	}

	skills.Register(skills.NewFeedback())
	skills.Register(skills.NewRAGQuery())
	// coding is registered before file-write and terminal for the same
	// reason its classifier is ordered ahead of theirs: a real "build me
	// software" request must reach the harness that designs, tests and
	// fixes it, not the one that saves a single unexecuted completion.
	if standalone {
		skills.Register(skills.NewCoding())
	}
	atomizer.CodingAvailable = standalone
	skills.Register(skills.NewDownload())
	skills.Register(skills.NewTerminal())
	skills.Register(skills.NewFileWrite())
	skills.Register(skills.NewWeb())
	// The operator works a goal out on this machine command by command -
	// the atomizer routes to it by task type "operator", never by a
	// classifier, so its place in the order only has to be ahead of the
	// fallback.
	skills.Register(skills.NewOperator())
	// general is the conversational catch-all - registered normally (it
	// matches task type "general", which atomizer.Classify() only ever
	// returns when nothing more specific already claimed the step) AND
	// set as the explicit fallback, so Pick() structurally can never
	// return nil for a real request - see skills.SetFallback's comment.
	generalSkill := skills.NewGeneral()
	skills.Register(generalSkill)
	skills.SetFallback(generalSkill)

	sched := scheduler.New(provider, models, ws, l2, dataRAG, fb, l1)
	sched.Brain = buildBrain(cfg, models.Default)
	sched.SkillLib = skillLib
	s := newServer(cfg, provider, ws, sched)
	s.storesOff = storesOff
	s.tracer = trace.New(filepath.Join(ws.Root(), "traces"))

	// Binds all interfaces (0.0.0.0), not just 127.0.0.1 - a real bug
	// caught running this inside a container: a process bound to
	// loopback-only is unreachable through Docker's own -p port mapping,
	// since an incoming connection arrives on the container's external
	// interface, not its loopback. Every container on the same Docker network
	// can reach it too (other desktops among them), so who may call is decided
	// by the guard (guard.go): the desktop's agent token on every API route,
	// JSON-only writes, and no request from another site.
	//
	// A native Windows worker is not in a container: it shares the person's
	// own computer, where a listener on every interface is reachable from
	// the network and makes Windows Firewall ask about it. The control plane
	// sets SOUL_HOST=127.0.0.1 there.
	if cfg.AgentToken == "" {
		log.Printf("[soul] DECURIX_AGENT_TOKEN is not set: every API route but /api/health is refused (%s)", noTokenRefusal)
	}
	addr := cfg.Host + ":" + cfg.Port
	log.Printf("[soul] listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, s.mux()))
}
