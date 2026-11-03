package practice

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
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Default per-case and boot timeouts.
const (
	DefaultCaseTimeout = 10 * time.Minute
	DefaultBootTimeout = 4 * time.Minute
)

// MaxCaseTimeout bounds a case's time however long the taught work is.
const MaxCaseTimeout = 60 * time.Minute

// perStep is how long a case may take for each step the demonstration recorded.
const perStep = 8 * time.Second

// caseTimeout is how long one case (or the calibration replay) may run: the
// configured time when set, else time in proportion to the length of what was
// taught, at least DefaultCaseTimeout and at most MaxCaseTimeout.
func (s *Service) caseTimeout(src qualification.PracticeSource) time.Duration {
	if s.CaseTimeout > 0 {
		return s.CaseTimeout
	}
	t := time.Duration(sourceSteps(src)) * perStep
	if t < DefaultCaseTimeout {
		return DefaultCaseTimeout
	}
	if t > MaxCaseTimeout {
		return MaxCaseTimeout
	}
	return t
}

// sourceSteps is the number of steps the pinned skill recorded; 0 when unknown.
func sourceSteps(src qualification.PracticeSource) int {
	raw, ok := src.Files[".decurix/skills/"+src.Name+".json"]
	if !ok || src.Kind != qualification.SourceSkill {
		return 0
	}
	var skill struct {
		Steps []json.RawMessage `json:"steps"`
	}
	if json.Unmarshal(raw, &skill) != nil {
		return 0
	}
	return len(skill.Steps)
}

func (s *Service) bootTimeout() time.Duration {
	if s.BootTimeout > 0 {
		return s.BootTimeout
	}
	return DefaultBootTimeout
}

// checkMemory refuses with a sentence when the machine is short of memory.
func (s *Service) checkMemory() error {
	if s.FreeMemory == nil {
		return nil
	}
	need := s.MinFreeBytes
	if need == 0 {
		need = DefaultMinFreeBytes
	}
	avail, err := s.FreeMemory()
	if err != nil {
		return fmt.Errorf("%w: %v", Sentence("Could not read this machine's free memory."), err)
	}
	if avail < need {
		return LowMemoryError{AvailMB: avail >> 20, NeedMB: need >> 20}
	}
	return nil
}

// Running reports whether a practice run is active in this process.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active) > 0
}

// StartRun starts a practice run of an approved suite. sourceSHA is the source
// as exported now (CheckCurrent). It refuses a second run, a stale suite and
// short memory before anything is created; the run itself goes on in the
// background and ends with FinishPracticeRun, which never enables anything.
func (s *Service) StartRun(ctx context.Context, suiteID, sourceSHA string) (qualification.PracticeRun, error) {
	if s.Store == nil || s.Sandbox == nil || s.Broker == nil || s.BrokerURL == nil || s.Workers == nil {
		return qualification.PracticeRun{}, Sentence("The practice runner is not set up.")
	}
	s.mu.Lock()
	if len(s.active) > 0 {
		s.mu.Unlock()
		return qualification.PracticeRun{}, ErrBusy
	}
	// Hold the slot while checking, so two starts cannot both pass.
	if s.active == nil {
		s.active = map[string]context.CancelFunc{}
	}
	s.active[""] = func() {}
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		delete(s.active, "")
		s.mu.Unlock()
	}
	su, err := s.Store.Suite(suiteID)
	if err != nil {
		release()
		return qualification.PracticeRun{}, err
	}
	if su.Status != qualification.SuiteApproved {
		release()
		return qualification.PracticeRun{}, qualification.ErrDenied
	}
	if err := s.checkMemory(); err != nil {
		release()
		return qualification.PracticeRun{}, err
	}
	if err := s.CheckCurrent(ctx, su, sourceSHA); err != nil {
		release()
		return qualification.PracticeRun{}, err
	}
	desk, err := s.Workers.Get(ctx, su.WorkerID)
	if err != nil {
		release()
		return qualification.PracticeRun{}, err
	}
	tree, treeSHA, err := s.Workers.PracticeAgentTree(ctx, su.WorkerID)
	if err != nil || treeSHA != su.Pins.AgentSHA {
		release()
		return qualification.PracticeRun{}, ErrStale
	}
	snapshot, _, err := s.Workers.PracticeSnapshot(ctx, su.WorkerID)
	if err != nil {
		release()
		return qualification.PracticeRun{}, err
	}
	snapshot, err = mergeSnapshot(snapshot, su.Source.Files)
	if err != nil {
		release()
		return qualification.PracticeRun{}, err
	}
	broker, err := s.BrokerURL(ctx)
	if err != nil {
		log.Printf("practice: broker: %v", err)
		release()
		return qualification.PracticeRun{}, fmt.Errorf("%w: %v", Sentence("The practice broker could not start."), err)
	}
	run, su, err := s.Store.BeginPracticeRun(su.ResponsibilityID, su.Revision)
	if err != nil {
		release()
		return run, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	delete(s.active, "")
	s.active[run.ID] = cancel
	s.mu.Unlock()
	spec := desktop.PracticeSpec{RunID: run.ID, Image: su.Pins.Image, Harness: su.Pins.Harness, Brain: su.Pins.Brain, Model: su.Pins.Model,
		MemoryMB: desk.MemoryMB, BrokerURL: broker, Hosts: su.Source.Hosts, AgentTree: tree, Snapshot: snapshot}
	if s.Vision != nil {
		spec.Vision = s.Vision(su.Pins.Brain)
	}
	go s.execute(runCtx, run, su, spec)
	return run, nil
}

// CancelRun records the cancellation durably, then stops the run's work.
func (s *Service) CancelRun(runID string) (qualification.PracticeRun, error) {
	run, err := s.Store.CancelPracticeRun(runID)
	if err != nil {
		return run, err
	}
	s.mu.Lock()
	cancel := s.active[runID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.Tasks != nil {
		_ = s.Tasks.CloseOpen(desktop.PracticeDesktopID(runID), "The owner cancelled the practice run.")
	}
	return run, nil
}

// mergeSnapshot adds the pinned source's files (keys under "$HOME/.decurix/")
// to the worker's state snapshot; the pinned bytes win over the live ones.
func mergeSnapshot(snapshot []byte, files map[string][]byte) ([]byte, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	if len(snapshot) > 0 {
		tr := tar.NewReader(bytes.NewReader(snapshot))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if err := tw.WriteHeader(h); err != nil {
				return nil, err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return nil, err
			}
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rel, ok := strings.CutPrefix(n, ".decurix/")
		if !ok || rel == "" || strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") {
			return nil, fmt.Errorf("a pinned source file is outside the agent's folder: %s", n)
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n])), ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// execute runs every case in order in one practice desktop and removes
// everything at the end, on cancel and on error.
func (s *Service) execute(ctx context.Context, run qualification.PracticeRun, su qualification.PracticeSuite, spec desktop.PracticeSpec) {
	workerID := desktop.PracticeDesktopID(run.ID)
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
			_ = s.Tasks.CloseOpen(workerID, "The practice run ended.")
		}
		s.mu.Lock()
		delete(s.active, run.ID)
		s.mu.Unlock()
	}()
	finish := func(status, note string) {
		_, _ = s.Store.FinishPracticeRun(run.ID, status, note)
	}
	maxTokens := s.MaxTokens
	if maxTokens <= 0 {
		maxTokens = MaxTokensFromEnv()
	}
	s.Broker.Begin(workerID, maxTokens)
	var err error
	d, err = s.Sandbox.CreatePractice(ctx, spec)
	if err != nil {
		log.Printf("practice: create desktop: %v", err)
		if errors.Is(err, desktop.ErrNoSecret) {
			// Said, not swallowed: a run that cannot be made safely is the control plane's fault, not the worker's.
			finish("error", "The practice desktop was not made: "+desktop.ErrNoSecret.Error()+".")
			return
		}
		finish("error", "The practice desktop could not be created.")
		return
	}
	if err = s.waitAgent(ctx, d); err != nil {
		log.Printf("practice: agent ready: %v", err)
		finish("error", "The practice desktop's agent never became ready.")
		return
	}
	if su.Source.WorkKind == qualification.WorkWeb {
		fit, why := s.calibrate(ctx, run, su, d)
		if ctx.Err() != nil {
			return
		}
		if !fit {
			finish("mock_unfit", "The mock app did not fit the recorded demonstration ("+why+"); this does not count against the worker.")
			return
		}
	}
	for i, c := range su.Cases {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-s.Broker.Exhausted():
			finish("budget", fmt.Sprintf("The run reached its cap of %d tokens; it does not count.", maxTokens))
			return
		default:
		}
		_ = s.Store.PracticeProgress(run.ID, i+1, c.ID, s.Broker.Tokens())
		if i > 0 || su.Source.WorkKind == qualification.WorkWeb {
			if err = s.Sandbox.ResetPractice(ctx, d.ContainerID, spec.Snapshot); err == nil {
				err = s.waitAgent(ctx, d)
			}
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("practice: reset: %v", err)
					finish("error", "The practice desktop could not be reset between cases.")
				}
				return
			}
		}
		f, files := s.runCase(ctx, run, su, d, c, workerID)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-s.Broker.Exhausted():
			finish("budget", fmt.Sprintf("The run reached its cap of %d tokens; it does not count.", maxTokens))
			return
		default:
		}
		if err := s.Store.RecordPracticeCase(run.ID, f, files); err != nil {
			log.Printf("practice: record case: %v", err)
			finish("error", "Could not keep a case's complete evidence; the run cannot qualify.")
			return
		}
	}
	_ = s.Store.PracticeProgress(run.ID, len(su.Cases), "", s.Broker.Tokens())
	finish("", "")
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
		case <-time.After(2 * time.Second):
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
		return fmt.Errorf("the agent answered %d", resp.StatusCode)
	}
	return nil
}

// wait returns the case's observation, or "" on timeout; it stops early on
// cancel or on the token cap.
func (s *Service) wait(ctx context.Context, obs <-chan Observation, timeout time.Duration) (Observation, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case o := <-obs:
		return o, nil
	case <-timer.C:
		return Observation{}, nil
	case <-s.Broker.Exhausted():
		return Observation{}, errors.New("token cap reached")
	case <-ctx.Done():
		return Observation{}, ctx.Err()
	}
}

// calibrate replays the source's own demonstration against an empty mock: the
// mock fits only when the replay ends done and checked. It is the first act of
// a web run; a misfit is the mock's fault, not the worker's.
func (s *Service) calibrate(ctx context.Context, run qualification.PracticeRun, su qualification.PracticeSuite, d desktop.PracticeDesktop) (bool, string) {
	mockID, err := s.Sandbox.StartPracticeMock(ctx, su.Pins.Image, run.ID, []byte(su.Mock), []byte("{}"))
	if err != nil {
		return false, "the mock app could not start"
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = s.Sandbox.UnpausePractice(clean, mockID)
		_ = s.Sandbox.RemovePractice(clean, mockID)
	}()
	route := tasks.NewRoute()
	if s.Tasks != nil {
		_, _ = s.Tasks.CommitTyped(route, tasks.Worker{ID: desktop.PracticeDesktopID(run.ID), Name: su.Source.Title, Engine: "practice", Profile: "practice"}, "Practice calibration: "+su.Source.Title, tasks.TypePractice)
	}
	obs := s.Broker.StartCase(route.TaskID)
	defer s.Broker.EndCase()
	path := "/skills/" + su.Source.Name + "/run"
	body := map[string]any{"task_id": route.TaskID, "params": recordedValues(su.Source)}
	if su.Source.Kind == qualification.SourceRecording {
		path, body = "/recordings/"+su.Source.Name+"/replay", map[string]any{"task_id": route.TaskID}
	}
	if err := s.post(ctx, d, path, body); err != nil {
		return false, "the replay could not start"
	}
	o, err := s.wait(ctx, obs, s.caseTimeout(su.Source))
	if err != nil {
		return false, err.Error()
	}
	if o.Kind != qualification.ObservedFinal || !o.Checked {
		why := "the replay ended as " + qualification.ObservedOutcome(o.Kind) + " without a checked result"
		if o.Kind == "" {
			why = fmt.Sprintf("the replay did not finish within %d minutes", int(s.caseTimeout(su.Source).Minutes()))
		}
		if said := strings.TrimSpace(o.Text); said != "" {
			why += `; the worker said: "` + clip(said, 300) + `"`
		}
		return false, why
	}
	return true, ""
}

// recordedValues are the values the demonstration used, for replaying a skill
// as it was shown: each parameter's default, else the first sample its step
// recorded. A recording replays its own values and needs none.
func recordedValues(src qualification.PracticeSource) map[string]string {
	out := map[string]string{}
	if src.Kind != qualification.SourceSkill {
		return out
	}
	raw, ok := src.Files[".decurix/skills/"+src.Name+".json"]
	if !ok {
		return out
	}
	var skill struct {
		Params []struct {
			Name    string `json:"name"`
			Default any    `json:"default"`
			Step    any    `json:"step"`
		} `json:"params"`
		Steps []struct {
			N       any   `json:"n"`
			Samples []any `json:"samples"`
		} `json:"steps"`
	}
	if json.Unmarshal(raw, &skill) != nil {
		return out
	}
	samples := map[string]string{}
	for _, st := range skill.Steps {
		if len(st.Samples) > 0 && st.Samples[0] != nil {
			samples[fmt.Sprint(st.N)] = fmt.Sprint(st.Samples[0])
		}
	}
	for _, p := range skill.Params {
		if p.Name == "" {
			continue
		}
		if p.Default != nil && fmt.Sprint(p.Default) != "" {
			out[p.Name] = fmt.Sprint(p.Default)
		} else if v, ok := samples[fmt.Sprint(p.Step)]; ok && v != "" {
			out[p.Name] = v
		}
	}
	return out
}

// runCase runs one case: inputs in, mock up, request through the agent's normal
// POST /request, wait for its report, pause, collect, verify in Go.
func (s *Service) runCase(ctx context.Context, run qualification.PracticeRun, su qualification.PracticeSuite, d desktop.PracticeDesktop, c qualification.PracticeCase, workerID string) (qualification.PracticeFinding, []qualification.PracticeFile) {
	started := time.Now()
	ev := qualification.PracticeEvidence{}
	var problems []string
	route := tasks.NewRoute()
	if s.Tasks != nil {
		if _, err := s.Tasks.CommitTyped(route, tasks.Worker{ID: workerID, Name: su.Source.Title, Engine: "practice", Profile: "practice"}, "Practice ("+c.Category+"): "+clip(c.Request, 200), tasks.TypePractice); err != nil {
			problems = append(problems, "The case's task record could not be opened.")
		}
	}
	var mockID string
	var state []byte
	var obs <-chan Observation
	func() {
		if err := s.Sandbox.CopyPracticeInputs(ctx, d.ContainerID, c.ID, c.Files); err != nil {
			ev.RunErr = err
			return
		}
		if su.Mock != "" {
			fixture := []byte(c.Fixture)
			id, err := s.Sandbox.StartPracticeMock(ctx, su.Pins.Image, run.ID, []byte(su.Mock), fixture)
			if err != nil {
				ev.RunErr = err
				return
			}
			mockID = id
		}
		obs = s.Broker.StartCase(route.TaskID)
		if err := s.post(ctx, d, "/request", map[string]string{"content": c.Request, "task_id": route.TaskID}); err != nil {
			ev.RunErr = err
			return
		}
		o, err := s.wait(ctx, obs, s.caseTimeout(su.Source))
		if err != nil {
			ev.RunErr = err
		}
		ev.Observation, ev.Text = o.Kind, o.Text
	}()
	// Freeze the desktop and the mock before reading anything, so the worker
	// cannot change what the checker sees.
	if err := s.Sandbox.PausePractice(ctx, d.ContainerID, mockID); err != nil {
		problems = append(problems, "The practice desktop could not be paused before collection.")
	} else {
		outputs, changed, collect, err := s.Sandbox.CollectPractice(ctx, d.ContainerID, c.ID, c.Files)
		if err != nil {
			problems = append(problems, "The case's files could not be collected.")
		}
		ev.Outputs, ev.ChangedInputs = outputs, changed
		problems = append(problems, collect...)
		if mockID != "" {
			var err error
			if state, err = s.Sandbox.PracticeMockState(ctx, mockID); err != nil {
				problems = append(problems, "The app's stored records could not be read.")
			} else if len(state) > 0 {
				if err := json.Unmarshal(state, &ev.MockState); err != nil {
					problems = append(problems, "The app's stored records are not readable.")
				}
			}
		}
	}
	drafts, attachments := s.Broker.EndCase()
	ev.Drafts = drafts
	ev.CollectProblems = problems
	ev.DurationMS = time.Since(started).Milliseconds()
	clean, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	_ = s.Sandbox.UnpausePractice(clean, d.ContainerID, mockID)
	if mockID != "" {
		_ = s.Sandbox.RemovePractice(clean, mockID)
	}
	done()
	f := qualification.VerifyPractice(c, ev)
	f.TaskID = route.TaskID
	files := evidenceFiles(c, ev, state, attachments)
	if s.Tasks != nil {
		kind := tasks.OutcomeError
		switch ev.Observation {
		case qualification.ObservedFinal:
			kind = tasks.OutcomeDone
		case qualification.ObservedQuery:
			kind = tasks.OutcomeQuery
		case qualification.ObservedApproval:
			kind = tasks.OutcomeApproval
		}
		verdict := "failed its independent checks"
		if f.Passed {
			verdict = "passed its independent checks"
		}
		checked := f.Passed
		_, _ = s.Tasks.RecordOutcome(workerID, route.TaskID, tasks.Outcome{Kind: kind, Reply: "Practice case " + c.ID + " " + verdict + ". The worker said: " + clip(ev.Text, 2000), Engine: "practice", Checked: &checked})
	}
	return f, files
}

// evidenceFiles lists what is kept of a case: inputs, outputs, the mock's state
// and the drafts with their attachments.
func evidenceFiles(c qualification.PracticeCase, ev qualification.PracticeEvidence, state []byte, attachments map[string][]byte) []qualification.PracticeFile {
	var files []qualification.PracticeFile
	add := func(role, name string, body []byte) {
		if qualification.FileName(name) && len(body) <= qualification.MaxFileBytes {
			files = append(files, qualification.PracticeFile{CaseID: c.ID, Role: role, Name: name, Body: body})
		}
	}
	for _, n := range sortedKeys(c.Files) {
		add(qualification.RoleInput, n, c.Files[n])
	}
	for _, n := range sortedKeys(ev.Outputs) {
		add(qualification.RoleOutput, n, ev.Outputs[n])
	}
	if len(state) > 0 {
		add(qualification.RoleMockState, "state.json", state)
	}
	for i, d := range ev.Drafts {
		b, _ := json.MarshalIndent(d, "", " ")
		add(qualification.RoleDraft, fmt.Sprintf("draft-%d.json", i+1), b)
	}
	for _, n := range sortedKeys(attachments) {
		add(qualification.RoleDraft, n, attachments[n])
	}
	return files
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
