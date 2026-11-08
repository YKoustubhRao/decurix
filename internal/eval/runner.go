package eval

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/practice"
	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Arm kinds: a model works the cases; the three calibration arms run none. oracle runs each case's scripted solution
// (it must pass every case), nothing claims "done" and does nothing, and ask asks one question about nothing in
// particular and stops (each must pass none).
const (
	ArmModel   = "model"
	ArmOracle  = "oracle"
	ArmNothing = "nothing"
	ArmAsk     = "ask"
)

// AskAnything is the ask arm's question: one any worker could ask of any case.
const AskAnything = "Before I go on: is there anything else I should know?"

// Limits of one matrix.
const (
	MaxArms        = 8
	MaxRepeats     = 20
	MaxQuestions   = 5
	maxLedgerBytes = 1 << 20
)

var armName = regexp.MustCompile(`^[a-z0-9][a-z0-9+@._:-]{0,63}$`)

// Arm pins everything that changes a worker's behaviour (D10).
type Arm struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Expert   bool   `json:"expert"`
	// AgentTreeRev names the agent tree ("" is the image's own); AgentTree is its tar, rooted at decurix_agent/.
	AgentTreeRev string `json:"agent_tree_rev,omitempty"`
	AgentTree    []byte `json:"-"`
	// Pinned when the arm starts.
	Image      string `json:"image,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	// Endpoint is the provider's endpoint when the run started; Served is the model the upstream's answers said
	// served the arm (the first seen in the run for its deployment).
	Endpoint         string `json:"endpoint,omitempty"`
	Served           string `json:"served,omitempty"`
	ExpertEndpoint   string `json:"expert_endpoint,omitempty"`
	AgentTreeSHA     string `json:"agent_tree_sha,omitempty"`
	ExpertDeployment string `json:"expert_deployment,omitempty"`
}

// Sandbox is the practice desktop, its mock and the two primitives an eval adds (*desktop.Manager).
type Sandbox interface {
	practice.Sandbox
	ExecPracticeAsWorker(ctx context.Context, containerID string, argv []string, timeout time.Duration) (int, []byte, error)
	DesktopImageID(ctx context.Context, profile string) (string, error)
	// VerifyGateQuestion checks that a question was signed by the desktop's own approval gate.
	VerifyGateQuestion(desktopID, requestID, question, sig string) bool
}

// Bundles pulls a case's run bundle off its practice desktop (*runs.Puller over desktop.PracticeHome), before the
// next case resets the desktop; the worker is the practice container's id.
type Bundles interface {
	Pull(ctx context.Context, ref runs.ExecutionRef) (runs.Bundle, error)
}

// Slot is the control plane's one practice slot (*practice.Service): Simulate and evals take turns.
type Slot interface {
	Reserve() (release func(), err error)
}

// Service runs matrices: arms × repeats × cases, sequentially, in one practice desktop per arm.
type Service struct {
	Store     *Store
	Sandbox   Sandbox
	Broker    *practice.Broker
	BrokerURL func(ctx context.Context) (string, error)
	Hub       Hub
	// Recorder keeps the expert's calls (the broker records the desktop's own).
	Recorder llmrec.Recorder
	Tasks    practice.Tasks
	// Bundles pulls each model case's bundle, so its row names its trajectory (nil: rows name none).
	Bundles  Bundles
	Practice Slot
	// Vision reports whether a hosted provider reads images.
	Vision func(provider string) bool
	// FreeMemory is the machine's available memory in bytes; a run refuses below MinFreeBytes (2.5 GB by default).
	FreeMemory   func() (uint64, error)
	MinFreeBytes uint64
	// Minute is how long a budget minute lasts (a minute; tests shorten it).
	Minute      time.Duration
	BootTimeout time.Duration

	mu     sync.Mutex
	active map[string]*activeRun
}

type activeRun struct {
	cancel   context.CancelFunc
	progress Progress
	// done is closed when the run has ended and released the practice slot.
	done chan struct{}
}

// Progress is where a running matrix is.
type Progress struct {
	Arm    string `json:"arm"`
	Repeat int    `json:"repeat"`
	Case   string `json:"case"`
	Done   int    `json:"done"`
	Total  int    `json:"total"`
}

// MatrixRequest is one matrix to run.
type MatrixRequest struct {
	SuiteTar []byte
	Arms     []Arm
	Repeats  int
	// Cases limits the run to these case ids, in the suite's order (empty: every case).
	Cases []string
	// CarryState asks for the run mode that keeps the desktop's state from case to case: the desktop is reset before
	// each repeat's first case only. Refused unless the suite says carry_state, and a carry_state suite is refused
	// without it: the two must agree, so neither mode is ever run by accident.
	CarryState bool
}

// ErrBusy is a second practice or eval run while one is active.
var ErrBusy = practice.ErrBusy

// BadRequest is a matrix the runner refuses before anything is made: safe to show as it is.
type BadRequest string

func (b BadRequest) Error() string { return string(b) }

// Start validates a matrix, takes the practice slot, stores the run and runs it in the background. It refuses a
// second run, short memory and a suite with problems before anything is created.
func (s *Service) Start(ctx context.Context, req MatrixRequest) (Run, error) {
	if s.Store == nil || s.Sandbox == nil || s.Broker == nil || s.BrokerURL == nil || s.Practice == nil {
		return Run{}, BadRequest("The eval runner is not set up.")
	}
	suite, err := Load(req.SuiteTar)
	if err != nil {
		return Run{}, err
	}
	cases, err := selectCases(suite, req.Cases)
	if err != nil {
		return Run{}, err
	}
	if err := checkCarry(suite, req.CarryState); err != nil {
		return Run{}, err
	}
	if req.Repeats < 1 || req.Repeats > MaxRepeats {
		return Run{}, BadRequest(fmt.Sprintf("Repeats must be between 1 and %d.", MaxRepeats))
	}
	arms, err := s.checkArms(req.Arms)
	if err != nil {
		return Run{}, err
	}
	release, err := s.Practice.Reserve()
	if err != nil {
		return Run{}, err
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
	if err := s.checkMemory(); err != nil {
		return Run{}, err
	}
	if err := s.pinDeployments(arms); err != nil {
		return Run{}, err
	}
	blob, err := s.Store.PutSuite(req.SuiteTar)
	if err != nil {
		return Run{}, err
	}
	now := s.Store.now()
	run := Run{ID: sqlitedb.NewID("e", 8), CreatedAt: now, StartedAt: now, SuiteName: suite.Name, SuiteSHA: suite.SHA, SuiteBlob: blob,
		Arms: arms, Cases: caseIDs(cases), Repeats: req.Repeats, Status: RunRunning, Note: graderNote(), CarryState: req.CarryState}
	if err := s.Store.Create(run); err != nil {
		return Run{}, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	if s.active == nil {
		s.active = map[string]*activeRun{}
	}
	s.active[run.ID] = &activeRun{cancel: cancel, progress: Progress{Total: len(arms) * req.Repeats * len(cases)}, done: make(chan struct{})}
	s.mu.Unlock()
	ok = true
	go s.execute(runCtx, run, suite, cases, arms, release)
	return run, nil
}

// checkCarry refuses a run whose mode the suite was not written for: carrying state needs a suite that says
// carry_state, and such a suite needs the run to ask for it.
func checkCarry(suite Suite, carry bool) error {
	switch {
	case carry && !suite.CarryState:
		return BadRequest("The suite does not carry state from case to case: only a suite whose suite.json says \"carry_state\": true runs that way.")
	case !carry && suite.CarryState:
		return BadRequest("The suite carries state from case to case (carry_state): run it with --carry-state, or its later cases miss what the earlier ones left.")
	}
	return nil
}

// pinDeployments resolves, once for the whole matrix, the deployment every model arm and the expert are served by: an
// arm that starts later is held to what the run started with, so a settings change mid-run shows as invalid rows,
// never as a quiet comparison of two models (review EVAL-1, B4).
func (s *Service) pinDeployments(arms []Arm) error {
	expert, expertEndpoint := "", ""
	if s.Hub != nil && s.Hub.Configured(ExpertProvider) {
		if dep, err := s.Hub.Resolve(ExpertProvider, ExpertModel); err == nil {
			expert, expertEndpoint = dep, s.endpoint(ExpertProvider)
		}
	}
	for i := range arms {
		arms[i].ExpertDeployment, arms[i].ExpertEndpoint = expert, expertEndpoint
		if arms[i].Kind != ArmModel {
			continue
		}
		arms[i].Endpoint = s.endpoint(arms[i].Provider)
		dep, err := s.Hub.Resolve(arms[i].Provider, arms[i].Model)
		if err != nil || dep == "" {
			return BadRequest(fmt.Sprintf("Arm %s: its model could not be resolved.", arms[i].Name))
		}
		arms[i].Deployment = dep
	}
	return nil
}

// endpoint is where the hub sends a provider's calls now ("" when it says nothing).
func (s *Service) endpoint(provider string) string {
	if s.Hub == nil {
		return ""
	}
	return s.Hub.Status()[provider].Endpoint
}

// servedPins holds, per deployment, the model the upstream first said served it in this run: a later answer naming
// another model under the same deployment is a model change.
type servedPins struct {
	mu  sync.Mutex
	pin map[string]string
}

// check pins served for deployment the first time, and reports whether it matches the pin.
func (p *servedPins) check(deployment, served string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pin == nil {
		p.pin = map[string]string{}
	}
	pinned, ok := p.pin[deployment]
	if !ok {
		p.pin[deployment] = served
		return served, true
	}
	return pinned, pinned == served
}

// graderNote names the control plane build that graded the run: the graders are its code, so a score is bound to it
// as well as to the case-set hash.
func graderNote() string {
	rev, dirty := "unknown", ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range info.Settings {
			switch kv.Key {
			case "vcs.revision":
				rev = clipText(kv.Value, 12)
			case "vcs.modified":
				if kv.Value == "true" {
					dirty = " (modified)"
				}
			}
		}
	}
	return "Graded by control plane build " + rev + dirty + "."
}

func caseIDs(cases []Case) []string {
	out := make([]string, len(cases))
	for i, c := range cases {
		out[i] = c.ID
	}
	return out
}

// selectCases is the suite's cases, limited to ids when given, in the suite's order.
func selectCases(s Suite, ids []string) ([]Case, error) {
	if len(ids) == 0 {
		return s.Cases, nil
	}
	want := map[string]bool{}
	for _, id := range ids {
		if _, ok := s.Case(id); !ok {
			return nil, BadRequest(fmt.Sprintf("The suite has no case %q.", clipText(id, 64)))
		}
		want[id] = true
	}
	var out []Case
	for _, c := range s.Cases {
		if want[c.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}

// checkArms validates the arms and fills their defaults.
func (s *Service) checkArms(arms []Arm) ([]Arm, error) {
	if len(arms) == 0 || len(arms) > MaxArms {
		return nil, BadRequest(fmt.Sprintf("A matrix needs between 1 and %d arms.", MaxArms))
	}
	seen := map[string]bool{}
	out := make([]Arm, len(arms))
	for i, a := range arms {
		if !armName.MatchString(a.Name) || seen[a.Name] {
			return nil, BadRequest(fmt.Sprintf("Arm %d needs a unique name of letters, digits and +@._:- .", i+1))
		}
		seen[a.Name] = true
		switch a.Kind {
		case ArmModel:
			if a.Provider == "" {
				a.Provider = ExpertProvider
			}
			if a.Model == "" {
				a.Model = "default"
			}
			if (a.Provider != "azure" && a.Provider != "claude") || (a.Model != "default" && a.Model != "fast") {
				return nil, BadRequest(fmt.Sprintf("Arm %s: the provider is azure or claude, the model default or fast.", a.Name))
			}
			if s.Hub == nil || !s.Hub.Configured(a.Provider) {
				return nil, BadRequest(fmt.Sprintf("Arm %s: the %s model is not set up.", a.Name, a.Provider))
			}
			// The expert reads every question, answering or not, so the required/other split is decided alike.
			if !s.Hub.Configured(ExpertProvider) {
				return nil, BadRequest("The scripted expert needs the Azure model set up.")
			}
		case ArmOracle, ArmNothing, ArmAsk:
			a.Provider, a.Model, a.Expert = ExpertProvider, "", false
		default:
			return nil, BadRequest(fmt.Sprintf("Arm %s: the kind is model, oracle, nothing or ask.", a.Name))
		}
		if len(a.AgentTree) > 0 {
			sum, err := desktop.TarFingerprint(a.AgentTree)
			if err != nil || !treeRooted(a.AgentTree) {
				return nil, BadRequest(fmt.Sprintf("Arm %s: the agent tree is not a tar rooted at decurix_agent/.", a.Name))
			}
			a.AgentTreeSHA = sum
			if a.AgentTreeRev == "" {
				a.AgentTreeRev = "upload"
			}
		} else {
			a.AgentTreeRev = ""
		}
		out[i] = a
	}
	return out, nil
}

// treeRooted reports whether every entry of an agent tree tar sits under decurix_agent/, as the image's
// /opt/decurix_agent is copied (desktop.PracticeAgentTree).
func treeRooted(b []byte) bool {
	tr := tar.NewReader(bytes.NewReader(b))
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n > 0
		}
		if err != nil {
			return false
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if name != "decurix_agent/" && !strings.HasPrefix(name, "decurix_agent/") || strings.Contains(name, "..") {
			return false
		}
		n++
	}
}

func (s *Service) checkMemory() error {
	if s.FreeMemory == nil {
		return nil
	}
	need := s.MinFreeBytes
	if need == 0 {
		need = practice.DefaultMinFreeBytes
	}
	avail, err := s.FreeMemory()
	if err != nil {
		return BadRequest("Could not read this machine's free memory.")
	}
	if avail < need {
		return practice.LowMemoryError{AvailMB: avail >> 20, NeedMB: need >> 20}
	}
	return nil
}

func (s *Service) minute() time.Duration {
	if s.Minute > 0 {
		return s.Minute
	}
	return time.Minute
}

func (s *Service) bootTimeout() time.Duration {
	if s.BootTimeout > 0 {
		return s.BootTimeout
	}
	return practice.DefaultBootTimeout
}

// Progress returns where a running matrix is; false when it is not running here.
func (s *Service) Progress(id string) (Progress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.active[id]
	if !ok {
		return Progress{}, false
	}
	return a.progress, true
}

func (s *Service) setProgress(id string, fn func(p *Progress)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.active[id]; ok {
		fn(&a.progress)
	}
}

// Wait returns once the run has ended here and released the practice slot (at once when it is not running in this
// process), or ctx's error.
func (s *Service) Wait(ctx context.Context, id string) error {
	s.mu.Lock()
	a := s.active[id]
	s.mu.Unlock()
	if a == nil {
		return nil
	}
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Running reports whether any matrix is running in this process.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active) > 0
}

// Cancel records the cancellation durably, then stops the run's work; the desktop and mock are removed as the run
// unwinds.
func (s *Service) Cancel(id string) (Run, error) {
	run, err := s.Store.Get(id)
	if err != nil {
		return run, err
	}
	if run.Status != RunRunning {
		return run, nil
	}
	if err := s.Store.Finish(id, RunCancelled, "Cancelled by a person."); err != nil {
		return run, err
	}
	s.mu.Lock()
	a := s.active[id]
	s.mu.Unlock()
	if a != nil {
		a.cancel()
	}
	return s.Store.Get(id)
}

// emptySnapshot is an empty ~/.decurix: an eval worker starts with no skills, memory or experience.
var emptySnapshot = func() []byte {
	var buf bytes.Buffer
	_ = tar.NewWriter(&buf).Close()
	return buf.Bytes()
}()

// execute runs every arm in turn and ends the run; the practice slot is released at the end, whatever happens.
func (s *Service) execute(ctx context.Context, run Run, suite Suite, cases []Case, arms []Arm, release func()) {
	defer func() {
		release()
		s.mu.Lock()
		if a := s.active[run.ID]; a != nil {
			a.cancel()
			close(a.done)
		}
		delete(s.active, run.ID)
		s.mu.Unlock()
	}()
	served := &servedPins{}
	var expert *Expert
	if len(arms) > 0 && arms[0].ExpertDeployment != "" {
		expert = &Expert{Hub: s.Hub, Recorder: s.Recorder, Deployment: arms[0].ExpertDeployment}
		_ = s.Store.AddNote(run.ID, "The expert is "+ExpertProvider+" "+expert.Deployment+".")
	}
	invalid := false
	for i := range arms {
		if ctx.Err() != nil {
			return
		}
		bad, err := s.runArm(ctx, run, suite, cases, &arms[i], i, expert, served)
		_ = s.Store.SetArms(run.ID, arms)
		invalid = invalid || bad
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("eval: run %s arm %s: %v", run.ID, arms[i].Name, err)
				_ = s.Store.Finish(run.ID, RunError, fmt.Sprintf("Arm %s stopped: %s", arms[i].Name, publicError(err)))
			}
			return
		}
	}
	note := ""
	if invalid {
		note = "invalid: a model changed during the run."
	}
	_ = s.Store.Finish(run.ID, RunDone, note)
}

// publicError is what a run's note may say of an error: its own sentence, or a fixed line.
func publicError(err error) string {
	var b BadRequest
	if errors.As(err, &b) {
		return string(b)
	}
	return practice.Public(err)
}

// runArm makes the arm's practice desktop, runs every repeat of every case in it and removes it. It reports whether
// any row was invalid.
func (s *Service) runArm(ctx context.Context, run Run, suite Suite, cases []Case, arm *Arm, index int, expert *Expert, served *servedPins) (bool, error) {
	image, err := s.Sandbox.DesktopImageID(ctx, "basic")
	if err != nil {
		return false, practice.Sentence("The desktop image could not be read.")
	}
	arm.Image = image
	broker, err := s.BrokerURL(ctx)
	if err != nil {
		return false, practice.Sentence("The practice broker could not start.")
	}
	armRun := fmt.Sprintf("%s-a%d", run.ID, index+1)
	desktopID := desktop.PracticeDesktopID(armRun)
	spec := desktop.PracticeSpec{RunID: armRun, Image: image, Harness: "thebot", Brain: arm.Provider, Model: arm.Model,
		MemoryMB: 2048, BrokerURL: broker, Hosts: suite.Hosts(), AgentTree: arm.AgentTree, Snapshot: emptySnapshot, LegacyGate: arm.AgentTreeRev != ""}
	if spec.Model == "" {
		spec.Model = "default"
	}
	if s.Vision != nil {
		spec.Vision = s.Vision(arm.Provider)
	}
	var maxTokens int64
	for _, c := range cases {
		maxTokens += suite.CaseBudget(c).MaxTokens
	}
	label := ""
	if arm.Kind == ArmModel && arm.Model != "default" {
		label = arm.Model
	}
	s.Broker.BeginModel(desktopID, maxTokens*int64(run.Repeats), label)
	var d desktop.PracticeDesktop
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 60*time.Second)
		defer done()
		if d.ContainerID != "" {
			_ = s.Sandbox.UnpausePractice(clean, d.ContainerID)
			_ = s.Sandbox.RemovePractice(clean, d.ContainerID)
		}
		_ = s.Sandbox.RemovePracticeNetwork(clean)
		s.Broker.End()
		if s.Tasks != nil {
			_ = s.Tasks.CloseOpen(desktopID, "The eval arm ended.")
		}
	}()
	if d, err = s.Sandbox.CreatePractice(ctx, spec); err != nil {
		if errors.Is(err, desktop.ErrNoSecret) {
			return false, practice.Sentence("The practice desktop was not made: " + desktop.ErrNoSecret.Error() + ".")
		}
		return false, practice.Sentence("The practice desktop could not be created.")
	}
	if err := s.waitAgent(ctx, d); err != nil {
		return false, practice.Sentence("The practice desktop's agent never became ready.")
	}
	// What the arm's model and the expert resolve to now, against the run's pins, once its desktop is ready to
	// work: a change since the run started makes every row of this arm invalid, calls or none.
	drift := ""
	if arm.Kind == ArmModel {
		if now, err := s.Hub.Resolve(arm.Provider, arm.Model); err != nil || now != arm.Deployment {
			drift = fmt.Sprintf("%s: the arm's model resolves to %q at its start, not %q as when the run started", modelChanged, now, arm.Deployment)
		} else if now := s.endpoint(arm.Provider); now != arm.Endpoint {
			drift = fmt.Sprintf("%s: the arm's provider is reached at another endpoint than when the run started", modelChanged)
		}
	}
	if expert != nil && drift == "" {
		if now, err := s.Hub.Resolve(ExpertProvider, ExpertModel); err != nil || now != expert.Deployment {
			drift = fmt.Sprintf("%s: the expert resolves to %q at this arm's start, not %q as when the run started", modelChanged, now, expert.Deployment)
		} else if now := s.endpoint(ExpertProvider); now != arm.ExpertEndpoint {
			drift = fmt.Sprintf("%s: the expert's provider is reached at another endpoint than when the run started", modelChanged)
		}
	}
	invalid := false
	first := true
	for rep := 1; rep <= run.Repeats; rep++ {
		for i, c := range cases {
			if ctx.Err() != nil {
				return invalid, nil
			}
			s.setProgress(run.ID, func(p *Progress) { p.Arm, p.Repeat, p.Case = arm.Name, rep, c.ID })
			// Every case starts from the snapshot (its desktop is new for the first), unless the run carries state:
			// then only a repeat's first case does, and its later cases find what the earlier ones left.
			if !first && (!run.CarryState || i == 0) {
				err := s.Sandbox.ResetPractice(ctx, d.ContainerID, emptySnapshot)
				if err == nil {
					err = s.waitAgent(ctx, d)
				}
				if ctx.Err() != nil {
					return invalid, nil
				}
				if err != nil {
					return invalid, practice.Sentence("The practice desktop could not be reset between cases.")
				}
			}
			first = false
			row := s.runCase(ctx, run, suite, *arm, armRun, d, c, rep, expert, served)
			if arm.Served == "" && len(row.Served) > 0 {
				arm.Served = row.Served[0]
			}
			if ctx.Err() != nil {
				return invalid, nil
			}
			if drift != "" {
				row.Invalid = true
				row.Problems = append(row.Problems, drift)
			}
			invalid = invalid || row.Invalid
			if err := s.Store.AddRow(row); err != nil {
				return invalid, fmt.Errorf("store a row: %w", err)
			}
			s.setProgress(run.ID, func(p *Progress) { p.Done++ })
		}
	}
	return invalid, nil
}

// waitAgent polls the practice desktop's agent until it answers.
func (s *Service) waitAgent(ctx context.Context, d desktop.PracticeDesktop) error {
	deadline := time.Now().Add(s.bootTimeout())
	for {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := s.Sandbox.PracticeAgentRequest(reqCtx, d, http.MethodGet, "/skills", nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
		}
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the agent did not answer in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// post sends one JSON request to the practice desktop's agent.
func (s *Service) post(ctx context.Context, d desktop.PracticeDesktop, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := s.Sandbox.PracticeAgentRequest(ctx, d, http.MethodPost, path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return agentRefused(resp.StatusCode)
	}
	return nil
}

// agentRefused is the worker's agent answering a delivered request with an error: the worker's failure.
type agentRefused int

func (a agentRefused) Error() string { return fmt.Sprintf("the agent answered %d", int(a)) }

// notDelivered records why something was not delivered: the agent's own refusal is the worker failing the case; a
// connection the runner could not make is infrastructure, counted apart.
func notDelivered(ev *Evidence, err error, what string) {
	var refused agentRefused
	if errors.As(err, &refused) {
		ev.Unfinished = append(ev.Unfinished, what+" The worker's agent answered "+fmt.Sprint(int(refused))+".")
		return
	}
	ev.Infra = append(ev.Infra, what)
}

// rowChecks is what a row keeps of how its case was decided.
type rowChecks struct {
	Expected   string  `json:"expected"`
	Graders    []Check `json:"graders"`
	Questions  []Asked `json:"questions"`
	WorkerSaid string  `json:"worker_said,omitempty"`
	// Verdict is the worker's own decurix.verdict/v1 on its final, as it sent it.
	Verdict json.RawMessage `json:"verdict,omitempty"`
	// Served is the model the upstream's answers said served the case.
	Served []string `json:"served,omitempty"`
}

// runCase runs one case in the arm's desktop and returns its row: inputs in, a fresh mock, the arm's work, then a
// pause before anything is read, collection, the grade, and the mock removed.
func (s *Service) runCase(ctx context.Context, run Run, suite Suite, arm Arm, armRun string, d desktop.PracticeDesktop, c Case, rep int, expert *Expert, served *servedPins) Row {
	started := time.Now()
	desktopID := desktop.PracticeDesktopID(armRun)
	budget := suite.CaseBudget(c)
	route := tasks.NewRoute()
	route.Origin = tasks.Origin{Kind: tasks.OriginEval, Ref: fmt.Sprintf("%s/%s/%d/%s", run.ID, arm.Name, rep, c.ID)}
	var problems []string
	if s.Tasks != nil {
		if _, err := s.Tasks.CommitTyped(route, tasks.Worker{ID: desktopID, Name: "Eval " + arm.Name, Engine: "eval", Profile: "practice"},
			"Eval ("+c.Category+"): "+clipText(suite.Withhold(c.Request), 200), tasks.TypePractice, route.Origin); err != nil {
			problems = append(problems, "The case's task record could not be opened.")
		}
	}
	ev := Evidence{}
	var mockID string
	obs := s.Broker.StartCase(route.TaskID)
	s.Broker.LimitCase(budget.MaxTokens)
	caseCtx, cancel := context.WithTimeout(ctx, time.Duration(budget.MaxMinutes*float64(s.minute())))
	func() {
		if err := s.Sandbox.CopyPracticeInputs(ctx, d.ContainerID, c.ID, c.Files); err != nil {
			ev.Infra = append(ev.Infra, "The case's input files could not be copied in.")
			return
		}
		if c.Start.Mock != "" {
			id, err := s.Sandbox.StartPracticeMock(ctx, arm.Image, armRun, suite.MockSources[c.Start.Mock], c.Fixture)
			if errors.Is(err, desktop.ErrMockNotAnswering) {
				ev.Infra = append(ev.Infra, fmt.Sprintf("The case's mock app did not answer on its port within %d s.", desktop.PracticeMockWaitSeconds))
				return
			}
			if err != nil {
				ev.Infra = append(ev.Infra, "The case's mock app could not start.")
				return
			}
			mockID = id
		}
		switch arm.Kind {
		case ArmModel:
			problems = append(problems, s.observeModel(caseCtx, d, desktopID, route.TaskID, c, arm, expert, obs, &ev)...)
		case ArmOracle:
			problems = append(problems, s.runOracle(caseCtx, d, desktopID, route.TaskID, c, &ev)...)
		case ArmAsk:
			problems = append(problems, s.askAnything(caseCtx, desktopID, route.TaskID, c, expert, &ev)...)
		default:
			yes := true
			ev.Observation, ev.FinalText, ev.Checked = ObservedFinal, "done", &yes
		}
	}()
	cancel()
	// Freeze the desktop and the mock before reading anything, so the worker cannot change what the graders see.
	if err := s.Sandbox.PausePractice(ctx, d.ContainerID, mockID); err != nil {
		ev.CollectProblems = append(ev.CollectProblems, "The practice desktop could not be paused before collection.")
	} else {
		outputs, changed, collect, err := s.Sandbox.CollectPractice(ctx, d.ContainerID, c.ID, c.Files)
		if err != nil {
			ev.CollectProblems = append(ev.CollectProblems, "The case's files could not be collected.")
		}
		ev.Outputs, ev.ChangedInputs = outputs, changed
		ev.CollectProblems = append(ev.CollectProblems, collect...)
		if mockID != "" {
			if state, err := s.Sandbox.PracticeMockState(ctx, mockID); err != nil {
				ev.CollectProblems = append(ev.CollectProblems, "The app's stored records could not be read.")
			} else if ev.MockState, err = mockState(state); err != nil {
				ev.CollectProblems = append(ev.CollectProblems, "The app's stored records are not readable.")
			}
		}
	}
	usage := s.Broker.CaseUsage()
	drafts, _ := s.Broker.EndCase()
	ev.Drafts = drafts
	clean, done := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	_ = s.Sandbox.UnpausePractice(clean, d.ContainerID, mockID)
	// The worker's own ledger is read once it runs again: it is worker-written, reported and never a pass (D8).
	ev.Ledger = s.readLedger(clean, d, route.TaskID)
	trajectory, why := s.pullBundle(clean, arm, d, route)
	if why != "" {
		problems = append(problems, why)
	}
	if mockID != "" {
		_ = s.Sandbox.RemovePractice(clean, mockID)
	}
	done()
	ev.Minutes = time.Since(started).Minutes()
	mode := Mode{Expert: arm.Expert || arm.Kind == ArmOracle}
	res := Grade(c, ev, mode)
	row := Row{RunID: run.ID, Arm: arm.Name, Repeat: rep, CaseID: c.ID, TaskID: route.TaskID, Observed: res.Observed,
		Checked: ev.Checked != nil && *ev.Checked, Passed: res.Passed, FalseDone: res.FalseDone, DoneUncheckedWrong: res.DoneUncheckedWrong,
		QRequired: res.Questions.Required, QOther: res.Questions.Other, TokensIn: usage.Input, TokensCached: usage.Cached, TokensOut: usage.Output,
		Calls: usage.Calls, Minutes: ev.Minutes, Deployments: usage.Deployments, At: s.Store.now(), Infra: res.Infra}
	row.Problems = append(append([]string{}, res.Problems...), problems...)
	row.Served = usage.Served
	row.TrajectoryID = trajectory
	if arm.Kind == ArmModel && servedByOther(usage.Deployments, arm.Deployment) {
		row.Invalid = true
		row.Problems = append(row.Problems, fmt.Sprintf("%s: calls served by %s, the run pinned %q", modelChanged, strings.Join(usage.Deployments, ", "), arm.Deployment))
	}
	if arm.Kind == ArmModel {
		for _, m := range usage.Served {
			if m == "" {
				continue // an answer that names no model pins nothing and changes nothing
			}
			if pinned, ok := served.check(arm.Deployment, m); !ok {
				row.Invalid = true
				row.Problems = append(row.Problems, fmt.Sprintf("%s: %q answered as %q, the run's first answer from it said %q", modelChanged, arm.Deployment, m, pinned))
			}
		}
	}
	if containsString(problems, modelChanged) {
		row.Invalid = true
	}
	for i := range row.Problems {
		row.Problems[i] = suite.Withhold(row.Problems[i])
	}
	questions := ev.Questions
	if questions == nil {
		questions = []Asked{}
	}
	checks, _ := json.Marshal(rowChecks{Expected: res.Expected, Graders: res.Checks, Questions: questions,
		WorkerSaid: clipText(ev.FinalText, 2000), Verdict: ev.Verdict, Served: usage.Served})
	row.Checks = json.RawMessage(suite.Withhold(string(checks)))
	s.recordOutcome(desktopID, route.TaskID, c, ev, res)
	return row
}

// servedByOther reports calls served by more than one deployment, or by one that is not the arm's.
func servedByOther(seen []string, pinned string) bool {
	if len(seen) > 1 {
		return true
	}
	return len(seen) == 1 && seen[0] != pinned
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// mockState reads a mock's state: {"collections": {...}} (or the collections themselves, as a Simulate mock writes).
func mockState(b []byte) (map[string][]map[string]any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var wrapped struct {
		Collections map[string][]map[string]any `json:"collections"`
	}
	if err := json.Unmarshal(b, &wrapped); err == nil && wrapped.Collections != nil {
		return wrapped.Collections, nil
	}
	var flat map[string][]map[string]any
	if err := json.Unmarshal(b, &flat); err != nil {
		return nil, err
	}
	return flat, nil
}

// recordOutcome closes the case's task record with what the graders decided; the worker's words are kept as said.
func (s *Service) recordOutcome(desktopID, taskID string, c Case, ev Evidence, res Result) {
	if s.Tasks == nil {
		return
	}
	kind := tasks.OutcomeError
	switch ev.Observation {
	case ObservedFinal:
		kind = tasks.OutcomeDone
	case ObservedQuery:
		kind = tasks.OutcomeQuery
	case ObservedApproval:
		kind = tasks.OutcomeApproval
	}
	verdict := "failed its graders"
	if res.Passed {
		verdict = "passed its graders"
	}
	// The grade is the eval's verdict, and checked is derived from it (H-CP-2). A case the runner could not run as
	// written, or whose evidence could not be read, is not graded: its check was not made.
	graded := !res.Infra && ev.RunErr == nil && len(ev.CollectProblems) == 0
	_, _ = s.Tasks.RecordOutcome(desktopID, taskID, tasks.Outcome{Kind: kind, Engine: "eval",
		Verdict: practice.GraderVerdict(tasks.ByEval, res.Passed, graded, "eval.graders", "Eval case "+c.ID+" "+verdict+".", "case:"+c.ID),
		Reply:   "Eval case " + c.ID + " " + verdict + ". The worker said: " + clipText(ev.FinalText, 2000)})
}

// readLedger reads the worker's record ledger for the task through the worker's own user; nil when there is none.
func (s *Service) readLedger(ctx context.Context, d desktop.PracticeDesktop, taskID string) []byte {
	path := "/home/headless/.decurix/computer/evidence/" + taskID + ".records.json"
	argv := []string{"/bin/sh", "-c", `[ -f "$1" ] || exit 3; head -c ` + fmt.Sprint(maxLedgerBytes+1) + ` -- "$1"`, "ledger", path}
	exit, out, err := s.Sandbox.ExecPracticeAsWorker(ctx, d.ContainerID, argv, 30*time.Second)
	if err != nil || exit != 0 || len(out) > maxLedgerBytes {
		return nil
	}
	return out
}

// pullBundle pulls a case's bundle (its recording, ledger, notes, soul trace) off the practice desktop before the next
// case resets it, and returns the execution id its trajectory is served by: for a model arm, and for the oracle, whose
// steps run as the worker under the case's task id (its gui and office steps record as a worker's do). The nothing
// and ask arms run nothing on the desktop, so there is no run to pull ("", ""). A pull that fails returns "" and says
// why, in a sentence of this file's own.
func (s *Service) pullBundle(ctx context.Context, arm Arm, d desktop.PracticeDesktop, route tasks.Route) (string, string) {
	if (arm.Kind != ArmModel && arm.Kind != ArmOracle) || s.Bundles == nil || s.Tasks == nil || route.ExecutionID == "" {
		return "", ""
	}
	_, err := s.Bundles.Pull(ctx, runs.ExecutionRef{ExecID: route.ExecutionID, TaskID: route.TaskID, Worker: d.ContainerID})
	switch {
	case err == nil:
		return route.ExecutionID, ""
	case errors.Is(err, runs.ErrNothingFound):
		return "", "The practice desktop held no part of the case's run, so the row names no trajectory."
	default:
		log.Printf("eval: pulling the bundle of %s: %v", route.TaskID, err)
		return "", "The case's run bundle could not be pulled, so the row names no trajectory."
	}
}

// observeModel sends the case's request and follows the worker turn by turn: a question gets the expert's chosen
// fact (or the fixed sentence) when the arm has the expert, and ends the case when it does not; an approval, a final
// or an error ends it; so do the case's minutes and its token budget.
func (s *Service) observeModel(ctx context.Context, d desktop.PracticeDesktop, desktopID, taskID string, c Case, arm Arm, expert *Expert, obs <-chan practice.Observation, ev *Evidence) []string {
	var problems []string
	if err := s.post(desktop.WithGateNotice(ctx, practice.RequestGateNotice(taskID, c.Request)), d, "/request", map[string]string{"content": c.Request, "task_id": taskID}); err != nil {
		notDelivered(ev, err, "The request could not be sent to the worker.")
		return problems
	}
	questions := 0
	exhausted := s.Broker.CaseExhausted()
	for {
		select {
		case o := <-obs:
			switch o.Kind {
			case qualification.ObservedQuery:
				questions++
				asked := Asked{Kind: ObservedQuery, Text: o.Text, Secret: o.Secret}
				if questions > MaxQuestions {
					ev.Questions = append(ev.Questions, asked)
					ev.Observation, ev.FinalText = ObservedQuery, o.Text
					ev.Unfinished = append(ev.Unfinished, "too many questions")
					return problems
				}
				if !o.Secret {
					problems = append(problems, s.classify(ctx, desktopID, taskID, c, expert, &asked)...)
				}
				if !arm.Expert || o.Secret {
					// No expert, or a secret nobody may answer with a value: the question ends the case.
					ev.Questions = append(ev.Questions, asked)
					ev.Observation, ev.FinalText = ObservedQuery, o.Text
					return problems
				}
				asked.Reply = Reply(c, asked.Fact)
				ev.Questions = append(ev.Questions, asked)
				if err := s.post(desktop.WithGateNotice(ctx, practice.ReplyGateNotice(taskID, asked.Reply, o.Text, o.Secret)), d, "/request", map[string]string{"content": asked.Reply, "task_id": taskID}); err != nil {
					ev.Observation, ev.FinalText = ObservedQuery, o.Text
					notDelivered(ev, err, "The expert's reply could not be sent to the worker.")
					return problems
				}
			case qualification.ObservedApproval:
				text := o.Text
				// Only a question the desktop's own gate signed is the gate's; the worker's own "may I?" is not.
				if o.Gate != "" && s.Sandbox.VerifyGateQuestion(desktopID, o.GateID, o.Gate, o.GateSig) {
					text, ev.GateQuestion = o.Gate, o.Gate
				}
				ev.Questions = append(ev.Questions, Asked{Kind: ObservedApproval, Text: text, Required: true})
				ev.Observation, ev.FinalText = ObservedApproval, text
				return problems
			case qualification.ObservedFinal, qualification.ObservedError:
				checked := o.Checked
				ev.Observation, ev.FinalText, ev.Checked, ev.Verdict = o.Kind, o.Text, &checked, o.Verdict
				return problems
			}
		case <-exhausted:
			ev.Observation = ObservedBudget
			return append(problems, "The case reached its token budget.")
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				ev.Observation = ObservedTimeout
				return append(problems, "The case ran out of time.")
			}
			ev.RunErr = ctx.Err()
			return problems
		}
	}
}

// classify asks the expert which fact a question is about (it never answers here) and marks the question required
// when that fact is; the problems are the expert's, said in the row. A question that names no fact asks nobody.
func (s *Service) classify(ctx context.Context, desktopID, taskID string, c Case, expert *Expert, asked *Asked) []string {
	var problems []string
	// Only the facts the question names by the case's own refers_to can be earned, and only they are shown to the
	// expert: a question that names none asks nobody, so no model can match it to a fact.
	named := c.FactsNamedBy(asked.Text)
	for _, f := range named {
		asked.Named = append(asked.Named, f.ID)
	}
	if expert != nil && len(named) > 0 {
		choice, err := expert.ChooseFor(ctx, desktopID, taskID, asked.Text, named)
		if err != nil && ctx.Err() == nil {
			log.Printf("eval: expert: %v", err)
		}
		asked.Fact = choice.Fact
		if choice.Problem != "" {
			problems = append(problems, "expert: "+choice.Problem)
			if strings.Contains(choice.Problem, modelChanged) {
				problems = append(problems, modelChanged)
			}
		}
	}
	if f, ok := c.Fact(asked.Fact); ok {
		asked.Required = f.IsRequired()
	}
	return problems
}

// askAnything is the ask calibration arm: it does nothing and asks AskAnything, which the expert reads as any
// question is read; no case may accept it.
func (s *Service) askAnything(ctx context.Context, desktopID, taskID string, c Case, expert *Expert, ev *Evidence) []string {
	asked := Asked{Kind: ObservedQuery, Text: AskAnything}
	problems := s.classify(ctx, desktopID, taskID, c, expert, &asked)
	ev.Questions = append(ev.Questions, asked)
	ev.Observation, ev.FinalText = ObservedQuery, AskAnything
	return problems
}

// oraclePrelude runs one oracle line as the worker would run a command in its desktop session: the session's
// display and bus, the office tools on the path, the case's folder as the working folder, the task's id, and
// MOCK_ROUTE for curl to reach the case's mock under its own host name.
const oraclePrelude = `export HOME=/home/headless USER=headless DISPLAY=:1 XAUTHORITY=/home/headless/.Xauthority NO_AT_BRIDGE=0 LANG=C.UTF-8
export PATH=/opt/office-venv/bin:/usr/local/bin:/usr/bin:/bin DECURIX_TASK_ID="$1" MOCK_ROUTE="--connect-to ::` + desktop.PracticeMockAlias + `:` + desktop.PracticeMockPort + `"
for p in /proc/[0-9]*; do
  if [ "$(cat "$p/comm" 2>/dev/null)" = xfce4-session ]; then
    d=$(tr '\0' '\n' < "$p/environ" 2>/dev/null | grep '^DBUS_SESSION_BUS_ADDRESS=' | head -n 1)
    [ -n "$d" ] && export "$d"
    break
  fi
done
cd "$2" || exit 97
eval "$3"`

// oracleContextScript makes the case's task the worker's current one, with its request, as the agent does when a
// request arrives (task_context.start). The solution's gui and office steps then meet the task's guards with the
// request a person sent, and record under the task, so its bundle has a run to pull. Without it the steps saw no task
// at all (the prelude's DECURIX_TASK_ID is read by nothing): a record could not be declared, nothing was recorded, and
// oracle rows named no trajectory (core-v2 calibration, round 8).
const oracleContextScript = `import sys; sys.path.insert(0, "/opt"); from decurix_agent import task_context; task_context.start(sys.argv[1], sys.argv[2])`

// oracleArgv is the command line one oracle step runs as.
func oracleArgv(taskID, caseID, line string) []string {
	return []string{"/bin/bash", "-c", oraclePrelude, "oracle", taskID, desktop.PracticeWorkDir + "/" + caseID, line}
}

// runOracle runs the case's scripted solution as the worker and records how it ended: an approval when the gate
// stopped an approval step, an error when a step exited otherwise than it should, else a checked final with the
// solution's answer.
func (s *Service) runOracle(ctx context.Context, d desktop.PracticeDesktop, desktopID, taskID string, c Case, ev *Evidence) []string {
	answer := "Done."
	if exit, _, err := s.Sandbox.ExecPracticeAsWorker(ctx, d.ContainerID, []string{"/usr/bin/python3", "-I", "-c", oracleContextScript, taskID, c.Request}, time.Minute); err != nil || exit != 0 {
		ev.Infra = append(ev.Infra, "The oracle's task could not be made the worker's current one.")
		return nil
	}
	for i, st := range c.Oracle {
		if ctx.Err() != nil {
			ev.Observation = ObservedTimeout
			return []string{"The case ran out of time."}
		}
		switch st.Kind {
		case StepAnswer:
			answer = st.Line
			continue
		case StepSleep:
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(st.Seconds * float64(time.Second))):
			}
			continue
		}
		remaining := time.Minute
		if dl, ok := ctx.Deadline(); ok {
			remaining = time.Until(dl)
		}
		exit, out, err := s.Sandbox.ExecPracticeAsWorker(ctx, d.ContainerID, oracleArgv(taskID, c.ID, st.Line), remaining)
		said := strings.TrimSpace(string(out))
		if err != nil {
			ev.Observation, ev.FinalText = ObservedError, said
			if ctx.Err() != nil {
				ev.Observation = ObservedTimeout
			}
			return []string{fmt.Sprintf("Oracle step %d could not run.", i+1)}
		}
		if st.Kind == StepApproval && exit == st.Exit {
			text := clipText(said, 2000)
			// The gate's own words for what it holds, as the agent sends them with an approval (approval.signed_question).
			if q := s.gateQuestion(ctx, d, desktopID, taskID, c.ID); q != "" {
				text, ev.GateQuestion = q, q
			}
			ev.Questions = append(ev.Questions, Asked{Kind: ObservedApproval, Text: text, Required: true})
			ev.Observation, ev.FinalText = ObservedApproval, text
			return nil
		}
		if exit != st.Exit {
			no := false
			ev.Observation, ev.FinalText, ev.Checked = ObservedError, said, &no
			return []string{fmt.Sprintf("Oracle step %d exited %d, not %d: %s", i+1, exit, st.Exit, clipText(lastLines(said, 5), 600))}
		}
	}
	yes := true
	ev.Observation, ev.FinalText, ev.Checked = ObservedFinal, answer, &yes
	return nil
}

// gateSignedQuestion prints the approval gate's pending request as the agent reads it for an approval note.
const gateSignedQuestion = `python3 -c 'import json, sys; sys.path.insert(0, "/opt"); from decurix_agent.computer import approval; print(json.dumps(approval.signed_question() or {}))'`

// gateQuestion is the desktop's gate's own question for the action it holds, when its signature checks; "" otherwise.
func (s *Service) gateQuestion(ctx context.Context, d desktop.PracticeDesktop, desktopID, taskID, caseID string) string {
	exit, out, err := s.Sandbox.ExecPracticeAsWorker(ctx, d.ContainerID, oracleArgv(taskID, caseID, gateSignedQuestion), 30*time.Second)
	if err != nil || exit != 0 {
		return ""
	}
	var q struct {
		ID       string `json:"id"`
		Question string `json:"question"`
		Sig      string `json:"sig"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &q) != nil || q.Question == "" {
		return ""
	}
	if !s.Sandbox.VerifyGateQuestion(desktopID, q.ID, q.Question, q.Sig) {
		return ""
	}
	return clipText(q.Question, 2000)
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
