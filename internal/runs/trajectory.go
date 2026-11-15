package runs

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Schema names a trajectory document.
const Schema = "decurix.trajectory/v1"

// Trajectory is one run's record, decurix.trajectory/v1 (docs/trajectory.md). JSON tags are the schema's names;
// a reader keeps fields it does not know.
type Trajectory struct {
	Schema    string  `json:"schema"`
	RunID     string  `json:"run_id"`
	TaskID    string  `json:"task_id"`
	Attempt   int     `json:"attempt"`
	Worker    Worker  `json:"worker"`
	Origin    Origin  `json:"origin"`
	Skill     Skill   `json:"skill"`
	Request   Request `json:"request"`
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at"`
	Status    string  `json:"status"`
	Outcome   string  `json:"outcome"`
	// Verdict is the execution's decurix.verdict/v1 as stored, or null for a row from before verdicts.
	Verdict    json.RawMessage `json:"verdict"`
	Models     []Model         `json:"models"`
	Usage      Usage           `json:"usage"`
	Steps      []Step          `json:"steps"`
	Calls      []Call          `json:"calls"`
	Questions  []Question      `json:"questions"`
	Judgements []Judgement     `json:"judgements"`
	Ledger     []LedgerRecord  `json:"ledger"`
	Labels     Labels          `json:"labels"`
	Integrity  Integrity       `json:"integrity"`
}

// Worker is who ran the run, and the hashes of what it ran.
type Worker struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Harness  string `json:"harness"`
	Brain    string `json:"brain"`
	Image    string `json:"image"`
	AgentSHA string `json:"agent_sha"`
	SoulSHA  string `json:"soul_sha"`
	RulesSHA string `json:"rules_sha"`
}

// Actor is who made a human input: id, roles and kind (person, worker, scripted, program).
type Actor struct {
	ID    string   `json:"id"`
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles"`
	Kind  string   `json:"kind"`
}

// Origin is what started the run (tasks.Origin kinds), and the person who did when one did.
type Origin struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Actor *Actor `json:"actor"`
}

// Skill is the taught skill that drove the run; empty when none did.
type Skill struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// Request is the request as the records keep it (withheld), with its hash and files.
type Request struct {
	Text   string        `json:"text"`
	SHA256 string        `json:"sha256"`
	Files  []RequestFile `json:"files"`
}

// RequestFile is one file sent with the request.
type RequestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Model is one deployment that served calls of the run, with its call count by purpose.
type Model struct {
	Provider   string         `json:"provider"`
	Deployment string         `json:"deployment"`
	Calls      int            `json:"calls"`
	Purposes   map[string]int `json:"purposes"`
}

// Usage is the run's tokens, summed over its calls.
type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	Calls             int   `json:"calls"`
}

// Call is one model call of the run, in order: a proxy row ("proxy"), or, for a brain the proxy never sees, a line
// of soul's trace ("soul_trace", which has no bodies).
type Call struct {
	I            int      `json:"i"`
	ID           string   `json:"id"`
	At           string   `json:"at"`
	Source       string   `json:"source"`
	Provider     string   `json:"provider"`
	Deployment   string   `json:"deployment"`
	Purpose      string   `json:"purpose"`
	Turn         string   `json:"turn"`
	Caller       string   `json:"caller"`
	Status       int      `json:"status"`
	Ms           int64    `json:"ms"`
	PromptSHA256 string   `json:"prompt_sha256"`
	PrefixSHA256 string   `json:"prefix_sha256"`
	ResponseSHA  string   `json:"response_sha256"`
	Messages     int      `json:"messages"`
	Usage        Usage    `json:"usage"`
	Problems     []string `json:"problems"`
}

// Step is one action the worker took (a recording event), or one command soul ran that left no event.
type Step struct {
	I            int          `json:"i"`
	At           string       `json:"at"`
	Turn         string       `json:"turn"`
	Call         *int         `json:"call"`
	Model        *StepModel   `json:"model"`
	PromptSHA256 string       `json:"prompt_sha256"`
	PrefixSHA256 string       `json:"prefix_sha256"`
	Observation  *Observation `json:"observation"`
	Command      *Command     `json:"command"`
	Action       *Action      `json:"action"`
	Gate         Gate         `json:"gate"`
	Diff         *Diff        `json:"diff"`
	LedgerDelta  []LedgerDiff `json:"ledger_delta"`
	Label        string       `json:"label"`
}

// StepModel is the deployment of the call that chose a step.
type StepModel struct {
	Provider   string `json:"provider"`
	Deployment string `json:"deployment"`
}

// Observation is what the worker saw before the step: its frame, the accessibility snapshot's hash, the window and
// the URL.
type Observation struct {
	Frame  string `json:"frame"`
	AX     string `json:"ax"`
	Window string `json:"window"`
	URL    string `json:"url"`
}

// Command is one command soul ran, as its trace keeps it (redacted).
type Command struct {
	Text         string `json:"text"`
	Tool         string `json:"tool"`
	Exit         *int   `json:"exit"`
	Ms           int64  `json:"ms"`
	OutputSHA256 string `json:"output_sha256"`
	// OutputHead is the output's first 2 KB as soul's trace keeps it (withheld); the rest is only in its hash.
	OutputHead string `json:"output_head"`
	Guard      string `json:"guard"`
}

// Action is the on-screen action of a recording event.
type Action struct {
	RecordID string          `json:"record_id"`
	Kind     string          `json:"kind"`
	Target   json.RawMessage `json:"target"`
	Text     *string         `json:"text"`
	Withheld json.RawMessage `json:"withheld"`
	Backend  string          `json:"backend"`
	Outcome  string          `json:"outcome"`
	Actor    string          `json:"actor"`
}

// Gate is what the approval gate decided about a step: not_needed, asked, granted, denied or refused.
type Gate struct {
	Decision    string `json:"decision"`
	RequestID   string `json:"request_id"`
	JudgementID string `json:"judgement_id"`
}

// Diff is what a step changed on the screen: its frames, the change said, and what was expected.
type Diff struct {
	Before string          `json:"before"`
	After  string          `json:"after"`
	Change string          `json:"change"`
	Expect json.RawMessage `json:"expect"`
}

// LedgerDiff is one ledger field a step changed (always [] in v1: the ledger does not name the event of its save).
type LedgerDiff struct {
	Record   string `json:"record"`
	Field    string `json:"field"`
	Intended string `json:"intended"`
	Shown    string `json:"shown"`
	Status   string `json:"status"`
}

// Question is one time the run stopped for a person, and the answer with its author.
type Question struct {
	InterventionID string  `json:"intervention_id"`
	Kind           string  `json:"kind"`
	AskedAt        string  `json:"asked_at"`
	Text           string  `json:"text"`
	Secret         bool    `json:"secret"`
	Answer         *Answer `json:"answer"`
}

// Answer is the reply to a question, with the judgement that is its record.
type Answer struct {
	JudgementID string `json:"judgement_id"`
	At          string `json:"at"`
	Text        string `json:"text"`
	Actor       *Actor `json:"actor"`
}

// Judgement is one human (or scripted) input on the run, by reference.
type Judgement struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Actor       Actor           `json:"actor"`
	At          string          `json:"at"`
	DerivedFrom string          `json:"derived_from"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// LedgerRecord is one record the worker declared and saved, with what it intended and what the app showed.
type LedgerRecord struct {
	Key    string        `json:"key"`
	Source string        `json:"source"`
	Status string        `json:"status"`
	Fields []LedgerField `json:"fields"`
}

// LedgerField is one field of a ledger record.
type LedgerField struct {
	Label    string `json:"label"`
	Intended string `json:"intended"`
	AtSave   string `json:"at_save"`
	Shown    string `json:"shown"`
	Status   string `json:"status"`
}

// Labels are the run's labels: its own verdict (Run: done, not_done, unchecked, stopped, or "" from before verdicts),
// a person's verdict, and the eval's when an eval made the run. A step's label is that step's own outcome; the run's
// is never copied onto its steps.
type Labels struct {
	Run           string          `json:"run"`
	PersonVerdict string          `json:"person_verdict"`
	Eval          json.RawMessage `json:"eval"`
}

// Integrity says what the record rests on: the bundle's hashes, what was not found, and what the worker's own user
// could have written (O-1).
type Integrity struct {
	BundleSHA256  string            `json:"bundle_sha256"`
	Parts         map[string]string `json:"parts"`
	Missing       []string          `json:"missing"`
	WorkerWritten []string          `json:"worker_written"`
}

// TaskSource is the task records Build reads.
type TaskSource interface {
	Get(taskID string) (tasks.Detail, error)
	JudgementsForTask(taskID string) ([]JudgementRow, error)
}

// CallSource is the proxy's call log Build reads: the rows of taskID made by worker's desktop between from and to.
type CallSource interface {
	CallsForTask(taskID, worker string, from, to time.Time) ([]CallRow, error)
}

// ExecutionLookup is an optional TaskSource method: the task an execution belongs to. Without it, Build finds the
// task through the execution's bundle, so an execution with neither is unknown.
type ExecutionLookup interface {
	TaskOfExecution(execID string) (string, error)
}

// EvalSource is the eval rows Build reads for labels.eval (*eval.Store): the row that names execID as its
// trajectory, else a row of taskID that names none (its bundle was never pulled). ok is false when no eval made the
// run.
type EvalSource interface {
	EvalLabel(execID, taskID string) (label EvalLabel, ok bool, err error)
}

// EvalLabel is labels.eval: the eval run, suite, case, arm and repeat that made the run, and what its graders
// decided. Invalid and infrastructure rows are said, never left out: a reader decides what to count.
type EvalLabel struct {
	RunID      string `json:"run_id"`
	Suite      string `json:"suite"`
	SuiteSHA   string `json:"suite_sha"`
	Case       string `json:"case"`
	Arm        string `json:"arm"`
	Repeat     int    `json:"repeat"`
	Observed   string `json:"observed"`
	Passed     bool   `json:"passed"`
	FalseDone  bool   `json:"false_done"`
	Invalid    bool   `json:"invalid"`
	Infra      bool   `json:"infra"`
	CarryState bool   `json:"carry_state"`
	// FromWorker is the desktop whose taught state the run's cases started from, and SnapshotSHA that state's
	// fingerprint; both empty for a run that started every case empty.
	FromWorker  string `json:"from_worker,omitempty"`
	SnapshotSHA string `json:"snapshot_sha,omitempty"`
}

// Sources is everything Build joins.
type Sources struct {
	Tasks TaskSource
	Calls CallSource
	Store *Store
	Blobs Blobs
	// Evals labels a run an eval case made (nil: labels.eval stays null).
	Evals EvalSource
}

// ErrUnknownExecution is an execution Build cannot find.
var ErrUnknownExecution = errors.New("runs: no such execution")
