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
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"thebot/internal/config"
	"thebot/internal/feedback"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/rag"
	"thebot/internal/scheduler"
	"thebot/internal/session"
	"thebot/internal/skills"
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
	Outcome     scheduler.Outcome
	Question    string
	Route       string
	Steps       []scheduler.StepRecord
	Resumed     bool
	Log         []string
	cancel      context.CancelFunc
	done        chan struct{}
	subscribers map[chan string]bool
}

func newJobRecord(id, prompt string) *jobRecord {
	return &jobRecord{ID: id, Prompt: prompt, Status: "running", done: make(chan struct{}), subscribers: map[chan string]bool{}}
}

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

func (j *jobRecord) appendLog(line string) {
	j.mu.Lock()
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

func (j *jobRecord) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"id": j.ID, "prompt": j.Prompt, "status": j.Status,
		"plan": j.Plan, "results": j.Results, "finalAnswer": j.FinalAnswer,
		"usedSkill": j.UsedSkill, "proposedSkill": j.ProposedSkill,
		"outcome": j.Outcome, "question": j.Question, "route": j.Route,
		"steps": j.Steps, "resumed": j.Resumed,
	}
}

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
	// at most one: the next message in it either resumes that job or, when
	// the host says it is not a reply, discards it.
	pausedMu sync.Mutex
	paused   map[string]*scheduler.Checkpoint
}

// pausedJobTTL is how long a paused job waits for its reply before the next
// message in the session is treated as a new request.
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
	writeJSON(w, 200, map[string]any{"ollama": s.provider.IsReachable(ctx), "ollamaHost": s.cfg.OllamaHost, "workspace": s.ws.Root()})
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
		writeJSON(w, 500, map[string]any{"error": err.Error()})
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
		writeJSON(w, 500, map[string]any{"error": err.Error()})
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

	writeJSON(w, 200, out)
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
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	pending, err := s.sched.SkillLib.ListPending()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
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
		writeJSON(w, 400, map[string]any{"error": "no skill library configured for this session"})
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
			writeJSON(w, 404, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"confirmed": skill})
	case "reject":
		if err := s.sched.SkillLib.Reject(slug); err != nil {
			writeJSON(w, 404, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"rejected": slug})
	default:
		writeJSON(w, 404, map[string]any{"error": "unknown action - use /confirm or /reject"})
	}
}

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
		// Reply says whether this message answers the session's paused
		// job. Unset means "if there is one"; false discards it.
		Reply *bool `json:"reply"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, 400, map[string]any{"error": "prompt is required"})
		return
	}

	id := genID()
	job := newJobRecord(id, body.Prompt)
	s.jobsMu.Lock()
	s.jobs[id] = job
	s.jobsMu.Unlock()

	// Real conversation history for this session, threaded into the job
	// before it runs - every prior turn is actually handed to the model.
	history := s.sessions.History(body.SessionID)

	req := scheduler.Request{Prompt: body.Prompt, Context: body.Context, WorkDir: body.WorkDir, History: history}
	if paused := s.takePaused(body.SessionID); paused != nil {
		if body.Reply == nil || *body.Reply {
			req.Resume = paused
			job.Resumed = true
			job.appendLog(fmt.Sprintf("scheduler: this message answers the paused job %q", truncateLog(paused.Prompt, 80)))
		} else {
			job.appendLog(fmt.Sprintf("scheduler: dropping the paused job %q - this is a new request", truncateLog(paused.Prompt, 80)))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	job.mu.Lock()
	job.cancel = cancel
	job.mu.Unlock()

	go func() {
		defer cancel()
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
			job.Route = string(result.Route)
			job.Steps = result.Steps
			if result.Prompt != "" {
				job.Prompt = result.Prompt
			}
		}
		finalAnswer := job.FinalAnswer
		job.mu.Unlock()

		if err == nil && result.Checkpoint != nil {
			s.storePaused(body.SessionID, result.Checkpoint)
		}

		// Record BOTH sides of the exchange so the next message in this
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
			go s.sched.L1.ConsiderExchange(context.Background(), body.Prompt, finalAnswer, job.appendLog)
		}

		job.finish()
	}()

	writeJSON(w, 200, map[string]any{"id": id})
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

func (s *server) mux() *http.ServeMux {
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
	return mux
}

// buildProvider selects the real chat brain and, separately, a real
// embedder for the Data RAG - LLM_PROVIDER governs chat; the embedder
// follows from it EXCEPT for Anthropic, which has no embeddings API at
// all, so that case falls back to Ollama (if configured and actually
// reachable) or disables RAG/feedback for this run rather than crashing.
// modelConcurrency is how many chat calls may run at once. A local Ollama
// answers one request per model at a time unless it was started with
// OLLAMA_NUM_PARALLEL, so the default there is one; the hosted APIs take
// several.
func modelConcurrency(cfg config.Config) int {
	if v, err := strconv.Atoi(os.Getenv("MODEL_CONCURRENCY")); err == nil && v > 0 {
		return v
	}
	if cfg.Provider == "ollama" {
		if v, err := strconv.Atoi(os.Getenv("OLLAMA_NUM_PARALLEL")); err == nil && v > 0 {
			return v
		}
		return 1
	}
	return 4
}

func buildProvider(cfg config.Config) (llm.Provider, llm.Models, llm.Embedder) {
	switch cfg.Provider {
	case "openai":
		p := llm.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIEmbedModel)
		return p, llm.DefaultModels(cfg.OpenAIModel).WithCoding(cfg.CodingModel), p // OpenAI does both chat and embed - one provider, no fallback needed
	case "anthropic":
		chat := llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel)
		var embedder llm.Embedder
		if cfg.OllamaHost == "" {
			log.Printf("[soul] no OLLAMA_HOST configured for embeddings (Anthropic has no embeddings API) - Data RAG and feedback are disabled for this run")
		} else {
			ollama := llm.NewOllama(cfg.OllamaHost)
			ollama.EmbedModel = cfg.EmbedModel
			if ollama.IsReachable(context.Background()) {
				embedder = ollama
				log.Printf("[soul] using Ollama at %s for embeddings only (Anthropic has no embeddings API)", cfg.OllamaHost)
			} else {
				log.Printf("[soul] Ollama not reachable at %s - Data RAG and feedback are disabled for this run", cfg.OllamaHost)
			}
		}
		return chat, llm.DefaultModels(cfg.AnthropicModel).WithCoding(cfg.CodingModel), embedder
	default: // "ollama"
		p := llm.NewOllama(cfg.OllamaHost)
		p.EmbedModel = cfg.EmbedModel
		return p, llm.DefaultModels(cfg.ChatModel).WithCoding(cfg.CodingModel), p
	}
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
	provider := llm.Limited(rawProvider, modelConcurrency(cfg))
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
	if embedder != nil {
		dataRAG, err = rag.New(ws.Root(), embedder, cfg.DoclingScript)
		if err != nil {
			log.Fatalf("[soul] Data RAG init failed: %v", err)
		}
		defer dataRAG.Close()

		fb, err = feedback.New(ws.Root(), embedder)
		if err != nil {
			log.Fatalf("[soul] feedback store init failed: %v", err)
		}
		defer fb.Close()
	}

	// L1: curated cross-session memory. Needs both a brain (to judge what
	// is worth keeping) and an embedder (to recall it later), so it only
	// exists when an embedder does - same nil-safe pattern as RAG.
	var l1 *memory.L1
	if embedder != nil {
		l1, err = memory.NewL1(ws.Root(), provider, embedder, models.Judgment)
		if err != nil {
			log.Fatalf("[soul] L1 memory init failed: %v", err)
		}
		defer l1.Close()
	}

	// SkillLib: acquired procedural memory. Same nil-safe pattern - needs
	// an embedder to match a future request against what's been learned.
	var skillLib *memory.SkillLibrary
	if embedder != nil {
		skillLib, err = memory.NewSkillLibrary(ws.Root(), embedder, models.Judgment)
		if err != nil {
			log.Fatalf("[soul] skill library init failed: %v", err)
		}
	}

	skills.Register(skills.NewFeedback())
	skills.Register(skills.NewRAGQuery())
	// coding is registered before file-write and terminal for the same
	// reason its classifier is ordered ahead of theirs: a real "build me
	// software" request must reach the harness that designs, tests and
	// fixes it, not the one that saves a single unexecuted completion.
	skills.Register(skills.NewCoding())
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
	sched.SkillLib = skillLib
	s := newServer(cfg, provider, ws, sched)

	// Binds all interfaces (0.0.0.0), not just 127.0.0.1 - a real bug
	// caught running this inside a container: a process bound to
	// loopback-only is unreachable through Docker's own -p port mapping,
	// since an incoming connection arrives on the container's external
	// interface, not its loopback. This does NOT weaken "embedded, not an
	// external API" - reachability is still entirely gated by whether
	// whoever runs the container chooses to publish this port (or proxies
	// it internally, as flowera's own control plane does), never by a
	// public listener outside the VM.
	addr := "0.0.0.0:" + cfg.Port
	log.Printf("[soul] listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, s.mux()))
}
