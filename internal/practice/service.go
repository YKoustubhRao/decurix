package practice

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Workers reads what a practice run pins from the worker's own desktop
// (*desktop.Manager). It never changes that desktop.
type Workers interface {
	Get(ctx context.Context, id string) (desktop.Desktop, error)
	QualificationImage(ctx context.Context, workerID string) (string, error)
	PracticeAgentTree(ctx context.Context, workerID string) ([]byte, string, error)
	PracticeSnapshot(ctx context.Context, workerID string) ([]byte, string, error)
}

// Sandbox is the practice desktop and its mock app (*desktop.Manager).
type Sandbox interface {
	CreatePractice(ctx context.Context, spec desktop.PracticeSpec) (desktop.PracticeDesktop, error)
	ResetPractice(ctx context.Context, id string, snapshot []byte) error
	CopyPracticeInputs(ctx context.Context, id, caseID string, files map[string][]byte) error
	StartPracticeMock(ctx context.Context, image, runID string, source, fixture []byte) (string, error)
	PracticeMockState(ctx context.Context, mockID string) ([]byte, error)
	PausePractice(ctx context.Context, ids ...string) error
	UnpausePractice(ctx context.Context, ids ...string) error
	CollectPractice(ctx context.Context, id, caseID string, inputs map[string][]byte) (map[string][]byte, []string, []string, error)
	RemovePractice(ctx context.Context, ids ...string) error
	RemovePracticeNetwork(ctx context.Context) error
	PracticeAgentRequest(ctx context.Context, d desktop.PracticeDesktop, method, path string, body io.Reader) (*http.Response, error)
}

// Tasks keeps each practice case's task record (*tasks.Store); practice tasks
// are tasks.TypePractice, which never count toward autonomy.
type Tasks interface {
	CommitTyped(route tasks.Route, worker tasks.Worker, text, taskType string) (tasks.Route, error)
	AddStep(workerID, taskID string, step tasks.Step) (bool, error)
	RecordOutcome(workerID, taskID string, outcome tasks.Outcome) (string, error)
	CloseOpen(workerID, why string) error
}

var (
	// ErrBusy is a second practice run while one is active (one per control plane).
	ErrBusy = Sentence("A practice run is already active; wait for it or cancel it.")
	// ErrStale is a suite whose pinned worker, image, agent, rules or source changed.
	ErrStale = Sentence("The worker's skill, image, agent, rules or model changed since this suite was approved. Draft and approve a new suite.")
	// ErrHostedOnly is a worker whose brain the broker cannot serve (the local model, for now).
	ErrHostedOnly = Sentence("Practice runs need a worker that reasons with a hosted model.")
)

// LowMemoryError refuses a run when free memory is short; it never stops the person's VM.
type LowMemoryError struct{ AvailMB, NeedMB uint64 }

func (e LowMemoryError) Error() string {
	return fmt.Sprintf("Only %d MB of memory is free and a practice run needs %d MB. Stop a worker you are not using, then try again.", e.AvailMB, e.NeedMB)
}

// DefaultMinFreeBytes is the free memory a practice run needs before it starts (2.5 GB).
const DefaultMinFreeBytes = 2560 << 20

// DefaultMaxTokens caps one run's model use when DECURIX_PRACTICE_MAX_TOKENS is unset.
const DefaultMaxTokens = 2_000_000

// MaxTokensFromEnv reads DECURIX_PRACTICE_MAX_TOKENS; unset or invalid gives the default.
func MaxTokensFromEnv() int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("DECURIX_PRACTICE_MAX_TOKENS")), 10, 64); err == nil && v > 0 {
		return v
	}
	return DefaultMaxTokens
}

// Service drafts suites and runs them. Construct it with its fields set; the
// zero values of the limits fall back to the documented defaults.
type Service struct {
	Store   *qualification.Store
	LLM     Chatter
	Offline Offline
	Workers Workers
	Sandbox Sandbox
	Tasks   Tasks
	Broker  *Broker
	// BrokerURL is where the practice desktop reaches the broker (BrokerListener.URL).
	BrokerURL func(ctx context.Context) (string, error)
	// Rules is the worker's business rules as text, pinned by hash and shown to the author.
	Rules func(workerID string) string
	// Vision reports whether a hosted provider reads images (from the llm hub status).
	Vision func(provider string) bool
	// FreeMemory is the machine's available memory in bytes (ollama.AvailableMemory).
	FreeMemory   func() (uint64, error)
	MinFreeBytes uint64
	MaxTokens    int64
	CaseTimeout  time.Duration
	BootTimeout  time.Duration
	// Seed picks a generator seed; tests fix it.
	Seed func() int64

	mu     sync.Mutex
	jobs   map[string]*DraftJob
	active map[string]context.CancelFunc
}

// DraftRequest asks for one suite for one pinned source.
type DraftRequest struct {
	WorkerID string
	Source   qualification.PracticeSource
	Count    int
}

// DraftJob is an authoring in progress or finished; the page polls it.
type DraftJob struct {
	ID         string    `json:"id"`
	WorkerID   string    `json:"worker_id"`
	SourceKind string    `json:"source_kind"`
	SourceName string    `json:"source_name"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	SuiteID    string    `json:"suite_id,omitempty"`
	Tokens     int64     `json:"tokens"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
}

// Draft job states.
const (
	JobAuthoring  = "authoring"
	JobGenerating = "generating"
	JobDone       = "done"
	JobFailed     = "failed"
)

func (s *Service) seed() int64 {
	if s.Seed != nil {
		return s.Seed()
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint64(b[:])%1_000_000_000) + 1
}

// Pins reads everything besides the suite that decides the worker's behaviour.
func (s *Service) Pins(ctx context.Context, workerID string) (qualification.PracticePins, error) {
	var p qualification.PracticePins
	if s.Workers == nil {
		return p, Sentence("The practice runner is not set up.")
	}
	desk, err := s.Workers.Get(ctx, workerID)
	if err != nil {
		return p, err
	}
	if desk.Brain != desktop.BrainAzure && desk.Brain != desktop.BrainClaude {
		return p, ErrHostedOnly
	}
	if p.Image, err = s.Workers.QualificationImage(ctx, workerID); err != nil {
		return p, err
	}
	if _, p.AgentSHA, err = s.Workers.PracticeAgentTree(ctx, workerID); err != nil {
		return p, err
	}
	rules := ""
	if s.Rules != nil {
		rules = s.Rules(workerID)
	}
	p.RulesSHA = qualification.SHA256Hex([]byte(rules))
	p.Harness, p.Brain, p.Model = desk.Harness, desk.Brain, desk.Model
	p.AuthorModel = AuthorProvider + "/" + AuthorModel
	return p, nil
}

// samePins compares what can change under a pinned suite (the author is history).
func samePins(a, b qualification.PracticePins) bool {
	a.AuthorModel, b.AuthorModel = "", ""
	return a == b
}

// CheckCurrent confirms a suite's pins and source still hold. On a change it
// moves the responsibility to needs_retesting (an enabled grant falls back to
// supervised) and returns ErrStale. sourceSHA is the source as exported now.
func (s *Service) CheckCurrent(ctx context.Context, su qualification.PracticeSuite, sourceSHA string) error {
	now, err := s.Pins(ctx, su.WorkerID)
	if err != nil {
		return err
	}
	if samePins(now, su.Pins) && sourceSHA == su.Source.SHA256 {
		return nil
	}
	if err := s.Store.InvalidatePractice(su.ResponsibilityID, "The worker's skill, image, agent, rules or model changed since this suite was approved; practise again.", false); err != nil {
		return err
	}
	return ErrStale
}

// clampCount keeps a requested case count between the required categories and the suite limit.
func clampCount(n int, src qualification.PracticeSource) int {
	min := len(qualification.RequiredCategories(src))
	if n <= 0 {
		n = 8
	}
	if n < min {
		n = min
	}
	if n > qualification.MaxPracticeCases {
		n = qualification.MaxPracticeCases
	}
	return n
}

// StartDraft begins authoring a suite in the background and returns its job.
func (s *Service) StartDraft(req DraftRequest) (DraftJob, error) {
	if req.Source.OutOfScope != "" {
		return DraftJob{}, qualification.ErrInvalid
	}
	job := &DraftJob{ID: sqlitedb.NewID("j", 12), WorkerID: req.WorkerID, SourceKind: req.Source.Kind, SourceName: req.Source.Name, Status: JobAuthoring, StartedAt: time.Now().UTC()}
	s.mu.Lock()
	if s.jobs == nil {
		s.jobs = map[string]*DraftJob{}
	}
	for id, j := range s.jobs {
		if !j.EndedAt.IsZero() && time.Since(j.EndedAt) > 24*time.Hour {
			delete(s.jobs, id)
		}
	}
	s.jobs[job.ID] = job
	copied := *job
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		su, err := s.Draft(ctx, req, func(status string, tokens int64) { s.updateJob(job.ID, status, tokens, "", "") })
		if err != nil {
			s.updateJob(job.ID, JobFailed, -1, Public(err), su.ID)
			return
		}
		s.updateJob(job.ID, JobDone, -1, "", su.ID)
	}()
	return copied, nil
}

func (s *Service) updateJob(id, status string, tokens int64, failure, suiteID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	if j == nil {
		return
	}
	j.Status = status
	if tokens >= 0 {
		j.Tokens = tokens
	}
	if failure != "" {
		j.Error = clip(failure, 600)
	}
	if suiteID != "" {
		j.SuiteID = suiteID
	}
	if status == JobDone || status == JobFailed {
		j.EndedAt = time.Now().UTC()
	}
}

// Job returns one authoring job.
func (s *Service) Job(id string) (DraftJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return DraftJob{}, false
	}
	return *j, true
}

// Draft authors and generates one suite and saves it as a draft for the owner.
// A generator that ran but failed its checks is still saved, with its problems,
// so the owner sees why it cannot be approved.
func (s *Service) Draft(ctx context.Context, req DraftRequest, progress func(status string, tokens int64)) (qualification.PracticeSuite, error) {
	if progress == nil {
		progress = func(string, int64) {}
	}
	src := req.Source
	if src.OutOfScope != "" {
		return qualification.PracticeSuite{}, Sentence("This source is out of scope: " + src.OutOfScope + ".")
	}
	pins, err := s.Pins(ctx, req.WorkerID)
	if err != nil {
		return qualification.PracticeSuite{}, err
	}
	rules := ""
	if s.Rules != nil {
		rules = s.Rules(req.WorkerID)
	}
	count := clampCount(req.Count, src)
	authored, err := Author(ctx, s.LLM, AuthorInput{Source: src, Rules: rules, Count: count, Required: qualification.RequiredCategories(src)})
	progress(JobGenerating, authored.Tokens)
	if err != nil {
		return qualification.PracticeSuite{}, err
	}
	if authored.Unsupported != "" {
		return qualification.PracticeSuite{}, Sentence("The author could not draft this honestly: " + clip(authored.Unsupported, 400))
	}
	seed := s.seed()
	cases, problems := s.tryDraft(ctx, pins.Image, src, authored, seed, count)
	// One repair round: the author sees what running its code showed. A failure
	// of the container itself is not the code's, so it is not sent back.
	if len(problems) > 0 && !offlineFailure(problems) {
		in := AuthorInput{Source: src, Rules: rules, Count: count, Required: qualification.RequiredCategories(src),
			Repair: &AuthorRepair{Generator: authored.Generator, Mock: authored.Mock, Problems: problems}}
		repaired, err := Author(ctx, s.LLM, in)
		tokens := authored.Tokens + repaired.Tokens
		progress(JobGenerating, tokens)
		if err == nil && repaired.Unsupported == "" {
			againCases, againProblems := s.tryDraft(ctx, pins.Image, src, repaired, seed, count)
			if len(againProblems) < len(problems) {
				authored, cases, problems = repaired, againCases, againProblems
			}
		}
		authored.Tokens = tokens
	}
	su := qualification.PracticeSuite{WorkerID: req.WorkerID, Source: src, Pins: pins, Generator: authored.Generator, Mock: authored.Mock,
		Seed: seed, Count: count, Cases: cases, Problems: problems}
	if su.Cases == nil {
		su.Cases = []qualification.PracticeCase{}
	}
	return s.Store.SaveDraft(su)
}

// tryDraft runs an authored generator and, for web work, smoke-tests its mock
// app against the recorded pages; it returns the cases and every problem.
func (s *Service) tryDraft(ctx context.Context, image string, src qualification.PracticeSource, authored AuthorOutput, seed int64, count int) ([]qualification.PracticeCase, []string) {
	cases, problems := GenerateCases(ctx, s.Offline, image, authored.Generator, seed, count)
	if src.WorkKind == qualification.WorkWeb && strings.TrimSpace(authored.Mock) != "" && !offlineFailure(problems) {
		problems = append(problems, CheckMock(ctx, s.Offline, image, authored.Mock, src, cases)...)
	}
	return cases, problems
}

// offlineFailure reports whether a problem list is the offline container's own
// failure rather than something the generator's code did.
func offlineFailure(problems []string) bool {
	for _, p := range problems {
		if strings.Contains(p, "could not run in the offline container") || strings.Contains(p, "could not be checked in the offline container") {
			return true
		}
	}
	return false
}

// Sentence is an error written for the owner: safe to show on the page as it is.
type Sentence string

func (s Sentence) Error() string { return string(s) }

// Public is what the page may show for err: its sentence when it carries one,
// else a fixed line (the detail goes to the control plane's log).
func Public(err error) string {
	var s Sentence
	if errors.As(err, &s) {
		return string(s)
	}
	var low LowMemoryError
	if errors.As(err, &low) {
		return low.Error()
	}
	log.Printf("practice: %v", err)
	return "The practice step failed; the control plane's log has the detail."
}
