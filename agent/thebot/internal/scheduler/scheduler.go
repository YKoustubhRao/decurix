// Package scheduler runs a job: it decides the job's shape through the
// atomizer, executes the resulting step graph, recovers from failures,
// pauses for a human when it must, and composes the answer.
//
// A job moves through these stages:
//
//  1. Recall - long-term memory relevant to the request and TheBot's own
//     live counters are fetched once, and folded with the host's standing
//     context into the one system frame every model call in the job opens
//     with (internal/prompt).
//  2. Shape - a confirmed acquired skill's recipe, or the atomizer's triage
//     and, for compound requests only, its plan.
//  3. Gate - a request that is itself a consequential act ("pay the
//     invoice") pauses for approval before anything runs.
//  4. Execute - steps run as a dependency graph: every step whose
//     dependencies have finished starts, up to MaxParallel at once, each
//     with every finished step's results in front of it. Model calls are
//     serialised underneath by the provider (llm.Limited), so parallelism
//     speeds up the network and shell work around them without making the
//     model thrash.
//  5. Recover - a failed step gets one grounded reasoning call (retry it
//     differently, decompose the fix, or ask the human); a step that still
//     fails and has work depending on it gets the plan's remainder replanned
//     around it, once.
//  6. Pause - a step that needs approval or an answer stops the job with a
//     Checkpoint. The next request in the session resumes it exactly where
//     it stopped, instead of starting the task over.
//  7. Compose - one step's own answer is returned as is; several steps'
//     results are synthesised into one reply by a final model call.
package scheduler

import (
	"context"
	"time"

	"thebot/internal/atomizer"
	"thebot/internal/feedback"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/rag"
	"thebot/internal/skills"
	"thebot/internal/workspace"
)

// Outcome is what a finished job means for whoever asked.
type Outcome string

const (
	// OutcomeAnswered - the job did what was asked; FinalAnswer is the reply.
	OutcomeAnswered Outcome = "answered"
	// OutcomeNeedsApproval - paused before a consequential action; Question
	// says what and why. A "yes" in the same session resumes it.
	OutcomeNeedsApproval Outcome = "needs_approval"
	// OutcomeNeedsInput - paused because only the human can unblock it;
	// Question is what it needs. Their next request resumes it.
	OutcomeNeedsInput Outcome = "needs_input"
	// OutcomeFailed - it ran to the end and could not do what was asked;
	// FinalAnswer says what happened.
	OutcomeFailed Outcome = "failed"
)

// StepState is where one step of a job's graph stands.
type StepState string

const (
	StepPending StepState = "pending"
	StepRunning StepState = "running"
	StepDone    StepState = "done"
	StepFailed  StepState = "failed"
	StepPaused  StepState = "paused"
	// StepBlocked - a step it depends on failed, so it never ran.
	StepBlocked StepState = "blocked"
	// StepSkipped - replaced by a replan, or never reached before the job
	// stopped.
	StepSkipped StepState = "skipped"
)

// StepRecord is the public view of one step, for the UI and the harness.
type StepRecord struct {
	ID         int       `json:"id"`
	Step       string    `json:"step"`
	Type       string    `json:"type"`
	Needs      []int     `json:"needs,omitempty"`
	State      StepState `json:"state"`
	Skill      string    `json:"skill,omitempty"`
	DurationMs int64     `json:"durationMs,omitempty"`
}

type Job struct {
	Prompt      string
	Plan        []skills.Subtask
	Results     []skills.Result
	FinalAnswer string
	Status      string // "done" | "error"
	// UsedSkill is set when a matched acquired skill's own recipe drove
	// this job's plan directly, instead of a fresh atomizer/LLM call -
	// real transparency for "did it actually reuse what it learned".
	UsedSkill string
	// ProposedSkill is set when this job's recovery succeeded in a way
	// worth remembering - a real, pending proposal now sitting on disk,
	// awaiting a human's confirm/reject before it is ever trusted again.
	ProposedSkill string

	Outcome Outcome
	// Question is what the human is being asked, when the job paused.
	Question string
	// Secret is set when the answer to Question is a secret (a value for a password field): the host withholds the
	// reply wherever it keeps it (review F-1).
	Secret bool
	// Route is the atomizer's decision about the request's shape.
	Route atomizer.RouteKind
	Steps []StepRecord
	// Checkpoint is set when the job paused; hand it back in
	// Request.Resume to carry on.
	Checkpoint *Checkpoint
}

// Request is what one job runs.
type Request struct {
	// Prompt is the task, and only the task.
	Prompt string
	// Context is the host's standing context - who this desktop is, its
	// persistent worker memory, the approval rules it runs under. It goes
	// into the system frame and nowhere else.
	//
	// It used to arrive glued onto the prompt, which broke nearly
	// everything downstream at once: the classifiers matched the safety
	// rules instead of the task, planning "hello, whats your name?" took
	// 97 seconds and the reply answered the safety rule ("NEEDS_APPROVAL:
	// Before sending any real-world consequences..."), and session history
	// and the job log recorded the whole blob as what the human said.
	Context string
	// WorkDir is where commands run and relative paths resolve.
	WorkDir string
	// History is the conversation so far, oldest first.
	History []llm.Message
	// Resume, when set, continues that paused job instead of starting a new
	// one, with Prompt as the human's reply to it.
	Resume *Checkpoint
	// Reading is the host's one reading of Prompt when it answers a paused approval (skills.Reading), or nil: set, it
	// decides whether the action was approved, not soul's own word list.
	Reading *skills.Reading
	// PlanAgreed is the host's word that the request is a stage of a plan the person agreed (skills.RunContext).
	PlanAgreed bool
	// EarlierSaid is the person's replies on the task before this request, each with its question (skills.RunContext).
	EarlierSaid []skills.Said
	// PickedFields is the taught task's fields filled by picking an application's entry (skills.RunContext).
	PickedFields []string
	// CollectOnly is the host's word that the job collects documents and enters nothing (skills.RunContext).
	CollectOnly bool
	// EnteredEarlier is what earlier jobs of the task entered and saved (skills.RunContext).
	EnteredEarlier []string
}

type Scheduler struct {
	Provider llm.Provider
	Models   llm.Models
	// Brain is what the model can do; handed to every skill (RunContext).
	Brain     *llm.Brain
	Workspace *workspace.Workspace
	L2        *memory.L2
	// RAG, Feedback and L1 are all optional (nil-safe) - a Scheduler built
	// without them (e.g. in unit tests) still runs, it just skips
	// auto-ingestion, feedback consultation and memory recall rather than
	// panicking.
	RAG      *rag.RAG
	Feedback *feedback.Store
	L1       *memory.L1
	// SkillLib is acquired procedural memory (see internal/memory.
	// SkillLibrary) - optional and nil-safe like the other tiers above.
	SkillLib *memory.SkillLibrary

	// MaxParallel bounds how many steps run at once. Zero reads
	// THEBOT_MAX_PARALLEL_STEPS, else 3.
	MaxParallel int
	// JobTimeout bounds a whole job. Zero reads THEBOT_JOB_TIMEOUT_SECONDS,
	// else 2 hours - under the Decurix harness's own backstop, so a job that
	// runs long is reported by TheBot with what it finished, rather than
	// abandoned by the harness with nothing. A loop is bounded by the turn
	// budgets and the progress check (skills/context.go), not by the clock:
	// found live (2026-10-23), a goal that knew enough to work 25 minutes
	// without asking was stopped mid-work by a 25-minute limit, where one that
	// asked every few minutes got a fresh 25 with each answer.
	JobTimeout time.Duration
	// MaxReplans bounds how many times one job's plan may be rewritten after
	// a failure. Zero means 1.
	MaxReplans int
}

func New(provider llm.Provider, models llm.Models, ws *workspace.Workspace, l2 *memory.L2, dataRAG *rag.RAG, fb *feedback.Store, l1 *memory.L1) *Scheduler {
	return &Scheduler{Provider: provider, Models: models, Workspace: ws, L2: l2, RAG: dataRAG, Feedback: fb, L1: l1}
}

func (s *Scheduler) maxParallel() int {
	if s.MaxParallel > 0 {
		return s.MaxParallel
	}
	return llm.EnvInt("THEBOT_MAX_PARALLEL_STEPS", 3)
}

func (s *Scheduler) jobTimeout() time.Duration {
	if s.JobTimeout > 0 {
		return s.JobTimeout
	}
	return time.Duration(llm.EnvInt("THEBOT_JOB_TIMEOUT_SECONDS", 7200)) * time.Second
}

func (s *Scheduler) maxReplans() int {
	if s.MaxReplans > 0 {
		return s.MaxReplans
	}
	return 1
}

// Execute runs one request end to end. onLog streams progress as it
// happens, never buffered until the end. The returned error is reserved for
// a job that could not be attempted at all; everything that happens while
// attempting it - including failure - is reported in the Job.
func (s *Scheduler) Execute(ctx context.Context, req Request, onLog func(string)) (*Job, error) {
	if onLog == nil {
		onLog = func(string) {}
	}
	ctx, cancel := context.WithTimeout(ctx, s.jobTimeout())
	defer cancel()

	e := newExecution(s, ctx, req, onLog)
	if !e.prepare() {
		// Nothing to run: a declined request-level approval turned into a
		// fresh request (prepare re-entered Execute for it).
		return e.handoff, nil
	}
	e.run()
	return e.finish(), nil
}
