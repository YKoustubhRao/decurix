package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"thebot/internal/atomizer"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/prompt"
	"thebot/internal/safety"
	"thebot/internal/skills"
	"thebot/internal/workspace"
)

// requestAction is the approval key for a request that was itself the
// consequential act, as opposed to one command inside it.
const requestAction = "(the whole request)"

const (
	pauseApproval = "approval"
	pauseInput    = "input"
)

// node is one step of a running job.
type node struct {
	step      atomizer.Step
	state     StepState
	result    skills.Result
	hasResult bool
	// superseded marks a failed step whose remaining work a replan took
	// over - its failure no longer decides the job's outcome.
	superseded bool
	resume     any
	reply      string
	// reading is the host's reading of reply when it answers an approval (skills.Reading), or nil.
	reading *skills.Reading
	// grant is the one action the person just said yes to for this step, good for one run of it: handed to the
	// step's next run (runContext) and gone from here at once, so it is never in a checkpoint and never outlives
	// that run.
	grant    string
	started  time.Time
	duration time.Duration
	// seq is the order the step finished in; later steps see earlier
	// results in this order.
	seq int
}

// Checkpoint is a paused job: everything needed to carry on from where it
// stopped when the human replies.
type Checkpoint struct {
	Prompt    string
	Context   string
	WorkDir   string
	Route     atomizer.Route
	System    llm.Message
	Kind      string // pauseApproval | pauseInput
	Action    string // what the approval is for
	Question  string
	PausedID  int // 0 when the job, not a step, is waiting
	Replans   int
	UsedSkill string
	CreatedAt time.Time
	// GateRequest and GateFingerprint are the approval gate's request for this paused approval, when the desktop has
	// a gate (soul's gate.go): the reply that resumes it is the gate's settlement of that one request.
	GateRequest     string
	GateFingerprint string
	// PlanAgreed is the paused request's own (Request.PlanAgreed): the reply that resumes it does not carry it.
	PlanAgreed bool

	nodes []node
	seq   int
}

// Approval reports what a paused approval is for: the command a step stopped on, or the whole request (command
// empty). ok is false when the job is not waiting on an approval.
func (c *Checkpoint) Approval() (command, request string, ok bool) {
	if c == nil || c.Kind != pauseApproval {
		return "", "", false
	}
	if c.Action == requestAction {
		return "", c.Prompt, true
	}
	return c.Action, "", true
}

type pause struct {
	node     *node
	kind     string
	action   string
	reason   string
	question string
}

type execution struct {
	s        *Scheduler
	ctx      context.Context
	req      Request
	log      func(string)
	planner  atomizer.Planner
	system   llm.Message
	recalled []string
	stats    string
	route    atomizer.Route

	nodes []*node
	// requestApproved is the person's yes, given just now, to a request that was itself the consequential act.
	requestApproved bool

	replans       int
	usedSkill     string
	proposedSkill string
	seq           int

	pause      *pause
	stopReason string
	wantReplan *node
	snap       map[string]workspace.FileState

	// handoff is the job that ran instead, when a declined request-level
	// approval turned the reply into a new request.
	handoff *Job
}

func newExecution(s *Scheduler, ctx context.Context, req Request, onLog func(string)) *execution {
	return &execution{s: s, ctx: ctx, req: req, log: onLog}
}

func (e *execution) setSystem(system llm.Message) {
	e.system = system
	e.planner = atomizer.Planner{Provider: e.s.Provider, Model: e.s.Models.Judgment, System: &e.system, OnLog: e.log}
}

func (e *execution) prepare() bool {
	if e.req.Resume != nil {
		return e.restore(e.req.Resume)
	}
	e.fresh()
	return true
}

// fresh shapes a new request into a step graph.
func (e *execution) fresh() {
	s, p := e.s, e.req.Prompt

	// Recall comes before shaping, because the frame it produces is what
	// every call from here on - planning included - opens with. Live
	// counters and the time of day only go into a conversational job's
	// frame; see prompt.Frame for why the rest keep theirs stable.
	e.recalled = s.recallL1(e.ctx, p, e.log)
	e.stats = s.selfStats(e.ctx)
	frame := prompt.Frame{Host: e.req.Context, Facts: e.recalled, Now: time.Now()}
	if atomizer.Triage(p).Kind == atomizer.RouteChat {
		frame.Stats, frame.WithTime = e.stats, true
	}
	e.setSystem(frame.Message())

	// Acquired skills before fresh planning: a confirmed skill is a proven
	// recipe for a request shaped like this one.
	//
	// A CODING request is the one exception. "Build a Python library that
	// implements an LRU cache" once matched an acquired skill whose trigger
	// was "create a flask server file in python", and the whole build was
	// replaced by that skill's single recorded step, `python3 -m venv`. No
	// two projects are the same project; what transfers between builds is
	// per-failure knowledge, which the coding harness consults mid-build.
	var steps []atomizer.Step
	if atomizer.Classify(p) == "coding" {
		e.log("skill library: skipping whole-request skill reuse for a coding task - the build harness consults memory per failure instead, which is the granularity that actually transfers")
	} else if matched := s.matchSkill(e.ctx, p, e.log); matched != nil {
		e.usedSkill = matched.Slug
		for i, text := range matched.Steps {
			st := atomizer.Step{ID: i + 1, Text: text, Type: atomizer.Classify(text)}
			if i > 0 {
				st.Needs = []int{i}
			}
			steps = append(steps, st)
		}
		e.route = atomizer.Route{Kind: atomizer.RoutePlan, TaskType: atomizer.Classify(p), Reason: "reusing acquired skill " + matched.Slug}
		e.log(fmt.Sprintf("Plan ready: %d step(s) to run (from the acquired skill).", len(steps)))
	}

	if steps == nil {
		plan := e.planner.Atomize(e.ctx, p)
		steps, e.route = plan.Steps, plan.Route
		if plan.IsTask {
			e.log(fmt.Sprintf("Plan ready: %d step(s) to run.", len(steps)))
		} else {
			e.log("Answering directly - no task steps needed.")
		}
	}

	for _, st := range steps {
		e.nodes = append(e.nodes, &node{step: st, state: StepPending})
	}

	// The request is itself the consequential act. Stop before any of it
	// runs; the plan is already made, so approving costs nothing extra.
	if concern := safety.AssessRequest(p); concern != nil {
		e.log("safety: " + concern.Reason + " - asking before doing anything")
		e.pause = &pause{kind: pauseApproval, action: requestAction, reason: concern.Reason}
	}
}

// restore rebuilds a paused job and applies the human's reply to it.
func (e *execution) restore(cp *Checkpoint) bool {
	reply := strings.TrimSpace(e.req.Prompt)
	// The host's one reading of the reply decides when it sent one (a "haan fine" no word list knows, a "yes,
	// provided ..." that grants nothing yet); the word list only stands in without it.
	approvedNow := safety.IsApproval(reply)
	if r := e.req.Reading; r != nil && strings.TrimSpace(r.Decision) != "" {
		approvedNow = strings.EqualFold(strings.TrimSpace(r.Decision), "yes")
	}

	e.req.Prompt = cp.Prompt
	if e.req.Context == "" {
		e.req.Context = cp.Context
	}
	if e.req.WorkDir == "" {
		e.req.WorkDir = cp.WorkDir
	}
	e.req.PlanAgreed = e.req.PlanAgreed || cp.PlanAgreed
	e.route, e.usedSkill, e.replans, e.seq = cp.Route, cp.UsedSkill, cp.Replans, cp.seq
	e.setSystem(cp.System)
	for i := range cp.nodes {
		n := cp.nodes[i]
		e.nodes = append(e.nodes, &n)
	}

	e.log(fmt.Sprintf("scheduler: resuming the paused job %q with your reply", truncate(cp.Prompt, 70)))

	switch {
	case cp.Kind == pauseApproval && cp.Action == requestAction:
		if !approvedNow {
			// Nothing ran yet, so there is nothing to carry on - the reply
			// is simply the next thing the human wants.
			e.log("scheduler: not approved - taking your reply as a new request instead")
			job, _ := e.s.Execute(e.ctx, Request{Prompt: reply, Context: e.req.Context, WorkDir: e.req.WorkDir, History: e.req.History}, e.log)
			e.handoff = job
			return false
		}
		e.requestApproved = true
		e.log("scheduler: approved - starting")

	case cp.Kind == pauseApproval:
		n := e.byID(cp.PausedID)
		if n == nil {
			break
		}
		if approvedNow {
			// A yes is this one action's, for the step that asked, once.
			n.grant = strings.TrimSpace(cp.Action)
			e.log(fmt.Sprintf("scheduler: approved `%s`", cp.Action))
		}
		n.state, n.reply, n.reading = StepPending, reply, e.req.Reading
		n.resume = n.result.Checkpoint
		if !approvedNow && n.resume == nil {
			// A step with no transcript to continue cannot do its job
			// without the action it was refused.
			n.state = StepFailed
			n.result.Output = fmt.Sprintf("Not run - `%s` was not approved. Your reply: %s", cp.Action, reply)
			n.result.Meta["declined"] = true
			e.log("scheduler: not approved - skipping that step")
		}

	case cp.PausedID != 0:
		n := e.byID(cp.PausedID)
		if n == nil {
			break
		}
		n.state, n.reply, n.reading = StepPending, reply, e.req.Reading
		if n.result.Checkpoint != nil {
			n.resume = n.result.Checkpoint
		} else {
			// No transcript to continue: the operator takes the step over,
			// with the failure and the human's answer in front of it.
			n.step.Text = fmt.Sprintf("%s\n\nAn earlier attempt at this failed:\n%s\n\nThe human says: %s",
				n.step.Text, truncate(n.result.Output, 1500), reply)
			n.step.Type = "operator"
		}

	default:
		id := e.maxID() + 1
		e.nodes = append(e.nodes, &node{state: StepPending, step: atomizer.Step{
			ID:   id,
			Type: "operator",
			Text: fmt.Sprintf("Finish this request: %s\n\nProgress so far:\n%s\nYou asked the human: %s\nThey replied: %s",
				cp.Prompt, e.progressText(), cp.Question, reply),
		}})
	}
	return true
}

func (e *execution) byID(id int) *node {
	for _, n := range e.nodes {
		if n.step.ID == id {
			return n
		}
	}
	return nil
}

func (e *execution) maxID() int {
	max := 0
	for _, n := range e.nodes {
		if n.step.ID > max {
			max = n.step.ID
		}
	}
	return max
}

// singleUse is one approval: the action the person said yes to, good for one run of exactly that action. Taken, it is
// gone - a retry, a second identical command or a resumed checkpoint is asked again.
type singleUse struct {
	mu     sync.Mutex
	action string
}

// take reports whether action is the approved one, and uses the approval up when it is.
func (g *singleUse) take(action string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.action == "" || strings.TrimSpace(action) != g.action {
		return false
	}
	g.action = ""
	return true
}

// approvalFor hands n's grant (if any) to its next run, as a single-use approval, and clears it from n.
func approvalFor(n *node) func(string) bool {
	if n.grant == "" {
		return nil
	}
	g := &singleUse{action: n.grant}
	n.grant = ""
	return g.take
}

func (e *execution) run() {
	if e.pause != nil && !e.requestApproved {
		return
	}
	e.pause = nil

	if e.s.RAG != nil && e.s.Workspace != nil {
		if snap, err := e.s.Workspace.Snapshot(); err == nil {
			e.snap = snap
		} else {
			e.log("rag: could not snapshot workspace before running: " + err.Error())
		}
	}

	for {
		e.runGraph()
		if e.pause != nil || e.stopReason != "" || e.ctx.Err() != nil || e.wantReplan == nil {
			break
		}
		if !e.replan() {
			break
		}
	}
	// A step that paused while another was with the person is asked now (review T-13).
	if e.stopReason == "" && e.ctx.Err() == nil {
		e.nextWaiting()
	}

	if e.pause == nil {
		for _, n := range e.nodes {
			if n.state == StepPending {
				n.state = StepSkipped
			}
		}
	}
}

type stepDone struct {
	n   *node
	res skills.Result
}

type readiness int

const (
	notReady readiness = iota
	ready
	blockedByDependency
)

func (e *execution) readiness(n *node) readiness {
	for _, id := range n.step.Needs {
		dep := e.byID(id)
		if dep == nil {
			continue
		}
		switch dep.state {
		case StepDone:
		case StepFailed, StepBlocked, StepSkipped:
			return blockedByDependency
		default:
			return notReady
		}
	}
	return ready
}

// runGraph starts every step whose dependencies are met, up to the
// parallel limit, and folds results in as they arrive until nothing more
// can start.
func (e *execution) runGraph() {
	limit := e.s.maxParallel()
	results := make(chan stepDone, limit)
	running := 0

	for {
		if e.pause == nil && e.stopReason == "" && e.wantReplan == nil && e.ctx.Err() == nil {
			for _, n := range e.nodes {
				if running >= limit {
					break
				}
				if n.state != StepPending {
					continue
				}
				switch e.readiness(n) {
				case blockedByDependency:
					n.state = StepBlocked
					e.log(fmt.Sprintf("scheduler: step %d cannot run - a step it needs did not succeed", n.step.ID))
					continue
				case notReady:
					continue
				}

				n.state, n.started = StepRunning, time.Now()
				running++
				if len(e.nodes) > 1 {
					e.log(fmt.Sprintf("scheduler: step %d/%d [%s] %s", n.step.ID, len(e.nodes), n.step.Type, truncate(n.step.Text, 80)))
				}
				rc := e.runContext(n)
				subtask := skills.Subtask{Step: n.step.Text, Type: n.step.Type}
				go func(n *node) {
					defer func() {
						if r := recover(); r != nil {
							results <- stepDone{n, skills.Result{
								Subtask: subtask, SkillName: "crashed",
								Output: fmt.Sprintf("the step crashed: %v", r), Meta: map[string]any{"fatal": true},
							}}
						}
					}()
					results <- stepDone{n, e.s.runStep(rc, subtask, e.planner)}
				}(n)
			}
		}

		if running == 0 {
			return
		}
		d := <-results
		running--
		e.complete(d.n, d.res)
	}
}

func (e *execution) runContext(n *node) skills.RunContext {
	var finished []*node
	for _, other := range e.nodes {
		if other.hasResult && other != n && other.seq > 0 {
			finished = append(finished, other)
		}
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].seq < finished[j].seq })
	prior := make([]skills.Result, 0, len(finished))
	for _, f := range finished {
		prior = append(prior, f.result)
	}

	s := e.s
	return skills.RunContext{
		Ctx: e.ctx, OnLog: e.log, OriginalPrompt: e.req.Prompt,
		PriorResults: prior, Provider: s.Provider, Brain: s.Brain, Model: s.Models.ForTaskType(n.step.Type), Workspace: s.Workspace,
		RAG: s.RAG, Feedback: s.Feedback, SessionHistory: e.req.History, RecalledFacts: e.recalled,
		SelfStats: e.stats,
		// Live memory handles: the coding harness queries L1 mid-build with
		// a real error message, and writes back what it had to research.
		L1: s.L1, SkillLib: s.SkillLib,
		System: e.system, WorkDir: e.req.WorkDir, Approved: approvalFor(n),
		Resume: n.resume, HumanReply: n.reply, HumanReading: n.reading, PlanAgreed: e.req.PlanAgreed,
	}
}

// complete folds one finished step into the job.
func (e *execution) complete(n *node, res skills.Result) {
	if res.Meta == nil {
		res.Meta = map[string]any{}
	}
	e.seq++
	n.result, n.hasResult, n.seq = res, true, e.seq
	n.duration = time.Since(n.started)
	n.resume, n.reply, n.reading, n.grant = nil, "", nil, ""
	if slug, ok := res.Meta["proposedSkill"].(string); ok && slug != "" {
		e.proposedSkill = slug
	}
	e.ingest()

	flag := func(key string) bool { v, _ := res.Meta[key].(bool); return v }

	switch {
	case flag("needsApproval"):
		n.state = StepPaused
		e.setPause(pauseFor(n))

	case res.Verified:
		n.state = StepDone

	default:
		n.state = StepFailed
		switch {
		case e.ctx.Err() != nil:
			e.stopReason = e.ctxReason()
		case flag("fatal"):
			e.stopReason = "a step failed in a way retrying cannot fix"
		case flag("needsUserInput"):
			if e.pause != nil {
				// Another step is already with the person: this one waits its turn, paused, and is asked next.
				n.state = StepPaused
			}
			e.setPause(pauseFor(n))
		case len(e.nodes) > 1 && e.replans < e.s.maxReplans() && e.wantReplan == nil:
			e.wantReplan = n
		}
	}
}

// pauseFor is the pause n's result asks for: its approval, or its question.
func pauseFor(n *node) *pause {
	res := n.result
	if v, _ := res.Meta["needsApproval"].(bool); v {
		action, _ := res.Meta["action"].(string)
		reason, _ := res.Meta["approvalReason"].(string)
		return &pause{node: n, kind: pauseApproval, action: action, reason: reason}
	}
	question, _ := res.Meta["question"].(string)
	if question == "" {
		question, _ = res.Meta["failureReasoning"].(string)
	}
	if question == "" {
		question = truncate(res.Output, 1500)
	}
	return &pause{node: n, kind: pauseInput, question: question}
}

// setPause makes p the job's pause unless one is already with the person: a second pause never overwrites the first
// (review T-13). Its step stays StepPaused and is asked after the first is answered (nextWaiting).
func (e *execution) setPause(p *pause) {
	if e.pause == nil {
		e.pause = p
		return
	}
	e.log(fmt.Sprintf("scheduler: step %d also needs you - it is asked after the question already with you", p.node.step.ID))
}

// nextWaiting makes the earliest-finished step still waiting to be asked the job's pause, when nothing else is.
func (e *execution) nextWaiting() {
	if e.pause != nil {
		return
	}
	var first *node
	for _, n := range e.nodes {
		if n.state == StepPaused && n.hasResult && (first == nil || n.seq < first.seq) {
			first = n
		}
	}
	if first != nil {
		e.pause = pauseFor(first)
	}
}

func (e *execution) ctxReason() string {
	if errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("ran out of time (the job limit is %s)", e.s.jobTimeout())
	}
	return "cancelled"
}

// replan rewrites the rest of the plan around a failed step.
func (e *execution) replan() bool {
	failed := e.wantReplan
	e.wantReplan = nil
	e.replans++

	e.log(fmt.Sprintf("scheduler: step %d failed even after recovery - replanning the rest of the job around it...", failed.step.ID))
	plan := e.planner.Replan(e.ctx, e.req.Prompt, e.progressText())

	if plan.Question != "" {
		e.pause = &pause{kind: pauseInput, question: plan.Question}
		return false
	}
	if len(plan.Steps) == 0 {
		e.log("scheduler: no workable new plan - finishing with what succeeded")
		return false
	}

	for _, n := range e.nodes {
		if n.state == StepPending || n.state == StepBlocked {
			n.state = StepSkipped
		}
	}
	failed.superseded = true

	offset := e.maxID()
	for _, st := range plan.Steps {
		st.ID += offset
		for i := range st.Needs {
			st.Needs[i] += offset
		}
		e.nodes = append(e.nodes, &node{step: st, state: StepPending})
	}
	return true
}

// progressText is a plain account of the job so far, for the replanner
// and for a job-level question's resume.
func (e *execution) progressText() string {
	var b strings.Builder
	for _, n := range e.nodes {
		fmt.Fprintf(&b, "%d. [%s] %s - ", n.step.ID, n.step.Type, truncate(n.step.Text, 200))
		switch n.state {
		case StepDone:
			fmt.Fprintf(&b, "succeeded:\n%s\n", indent(truncate(n.result.Output, 500)))
		case StepFailed:
			fmt.Fprintf(&b, "FAILED:\n%s\n", indent(truncate(n.result.Output, 700)))
		case StepBlocked:
			b.WriteString("not run (it needed a step that failed)\n")
		case StepSkipped:
			b.WriteString("replaced\n")
		default:
			b.WriteString("not run yet\n")
		}
	}
	return b.String()
}

func indent(s string) string {
	return "   " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n   ")
}

// ingest keeps the Data RAG in step with the workspace: whatever changed
// since the last look is indexed now. One rolling snapshot, taken here on
// the scheduler's own goroutine, rather than a before/after pair around
// each step - with steps running side by side, a per-step diff would
// ingest a neighbour's files, twice.
func (e *execution) ingest() {
	if e.s.RAG == nil || e.snap == nil || e.ctx.Err() != nil {
		return
	}
	after, err := e.s.Workspace.Snapshot()
	if err != nil {
		e.log("rag: could not snapshot workspace after running: " + err.Error())
		return
	}
	changed := workspace.DiffNewOrChanged(e.snap, after)
	e.snap = after
	total := len(changed)
	for i, relPath := range changed {
		// "rag-progress:" is a parseable contract the soul UI renders as a
		// live progress bar - i/total are the real position in the diff.
		e.log(fmt.Sprintf("rag-progress: ingesting (%d/%d): %s", i+1, total, relPath))
		chunks, method, err := e.s.RAG.IngestFile(e.ctx, relPath)
		if err != nil {
			e.log(fmt.Sprintf("rag: P0 ingest failed for %s: %s", relPath, err.Error()))
			continue
		}
		e.log(fmt.Sprintf("rag-progress: ingested (%d/%d): %s - %d chunk(s) via %s", i+1, total, relPath, chunks, method))
	}
}

func (e *execution) checkpoint() *Checkpoint {
	cp := &Checkpoint{
		Prompt: e.req.Prompt, Context: e.req.Context, WorkDir: e.req.WorkDir,
		Route: e.route, System: e.system,
		Kind: e.pause.kind, Action: e.pause.action, Question: e.pause.question,
		Replans: e.replans, UsedSkill: e.usedSkill, CreatedAt: time.Now(), seq: e.seq, PlanAgreed: e.req.PlanAgreed,
	}
	if e.pause.node != nil {
		cp.PausedID = e.pause.node.step.ID
	}
	// No approval is carried: a yes was for one run of one action, and a resumed job asks again.
	for _, n := range e.nodes {
		c := *n
		c.grant = ""
		cp.nodes = append(cp.nodes, c)
	}
	return cp
}

// finish turns the execution into the Job the caller sees.
func (e *execution) finish() *Job {
	job := &Job{
		Prompt: e.req.Prompt, Status: "done", Route: e.route.Kind,
		UsedSkill: e.usedSkill, ProposedSkill: e.proposedSkill,
	}
	for _, n := range e.nodes {
		job.Plan = append(job.Plan, skills.Subtask{Step: n.step.Text, Type: n.step.Type})
		if n.hasResult {
			job.Results = append(job.Results, n.result)
		}
		job.Steps = append(job.Steps, StepRecord{
			ID: n.step.ID, Step: n.step.Text, Type: n.step.Type, Needs: n.step.Needs,
			State: n.state, Skill: n.result.SkillName, DurationMs: n.duration.Milliseconds(),
		})
	}

	if e.pause != nil {
		job.Checkpoint = e.checkpoint()
		if e.pause.kind == pauseApproval {
			job.Outcome = OutcomeNeedsApproval
			job.Question = approvalQuestion(e.pause, e.req.Prompt)
		} else {
			job.Outcome = OutcomeNeedsInput
			job.Question = e.pause.question
		}
		job.FinalAnswer = job.Question
	} else {
		job.Outcome, job.FinalAnswer = e.compose()
	}

	if e.s.L2 != nil {
		_ = e.s.L2.Append(memory.L2Entry{Prompt: job.Prompt, Status: string(job.Outcome), Plan: stepStrings(job.Plan)})
	}
	return job
}

func approvalQuestion(p *pause, request string) string {
	if p.action == requestAction {
		return fmt.Sprintf("Before I do anything: %s.\n\n> %s\n\nReply **yes** and I'll go ahead, or tell me what you'd like instead.", p.reason, truncate(request, 400))
	}
	return fmt.Sprintf("I need your OK before I run this, because %s:\n\n```\n%s\n```\n\nReply **yes** to run it, or tell me what to do instead.", p.reason, p.action)
}

func stepStrings(plan []skills.Subtask) []string {
	out := make([]string, len(plan))
	for i, s := range plan {
		out[i] = s.Step
	}
	return out
}
