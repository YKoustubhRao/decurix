package runs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// callWindowAfter is how long after an execution ends a call is still its: a last review or compaction can land
// after the reply.
const callWindowAfter = time.Minute

// maxPartRead bounds one bundle file Build parses.
const maxPartRead = 64 << 20

// Build assembles the trajectory of execID from src: the task records, the judgements, the proxy's calls of that
// execution's worker in its time window, and the bundle pulled off the desktop. A run with no bundle is still built,
// with no steps, and integrity.missing says "no bundle".
func Build(ctx context.Context, src Sources, execID string) (Trajectory, error) {
	return build(ctx, src, execID, viewOf)
}

// build is Build with the execution's view made by view (tests give the phase-2 fields that way).
func build(ctx context.Context, src Sources, execID string, view func(tasks.Execution) execView) (Trajectory, error) {
	if src.Tasks == nil || src.Store == nil {
		return Trajectory{}, errors.New("runs: Build needs the task records and the run store")
	}
	blobs := src.Blobs
	if blobs == nil {
		blobs = src.Store.blobs
	}
	bundle, hasBundle, err := src.Store.Bundle(execID)
	if err != nil {
		return Trajectory{}, err
	}
	taskID := bundle.TaskID
	if !hasBundle {
		lookup, ok := src.Tasks.(ExecutionLookup)
		if !ok {
			return Trajectory{}, ErrUnknownExecution
		}
		if taskID, err = lookup.TaskOfExecution(execID); err != nil || taskID == "" {
			return Trajectory{}, ErrUnknownExecution
		}
	}
	detail, err := src.Tasks.Get(taskID)
	if errors.Is(err, tasks.ErrNotFound) {
		return Trajectory{}, ErrUnknownExecution
	}
	if err != nil {
		return Trajectory{}, err
	}
	var exec *tasks.Execution
	for i := range detail.Executions {
		if detail.Executions[i].ID == execID {
			exec = &detail.Executions[i]
		}
	}
	if exec == nil {
		return Trajectory{}, ErrUnknownExecution
	}
	judgements, err := src.Tasks.JudgementsForTask(taskID)
	if err != nil {
		return Trajectory{}, err
	}
	var calls []CallRow
	if src.Calls != nil {
		to := time.Time{}
		if !exec.EndedAt.IsZero() {
			to = exec.EndedAt.Add(callWindowAfter)
		}
		rows, err := src.Calls.CallsForTask(taskID, exec.WorkerID, exec.StartedAt, to)
		if err != nil {
			return Trajectory{}, err
		}
		// Only this worker's own calls: a task id in a header is the desktop's word, never trusted for a join.
		for _, r := range rows {
			if r.DesktopID == exec.WorkerID {
				calls = append(calls, r)
			}
		}
	}
	var b *Bundle
	if hasBundle {
		b = &bundle
	}
	return assemble(runInput{detail: detail, view: view(*exec), judgements: judgements, calls: calls,
		bundle: b, blobs: blobs}), nil
}

// execView is one execution with the phase-2 fields the run record reads: each intervention's reply judgement and
// replier (CP-3), and the verdict, origin, skill and resolved model (CP-4/5). Those four are read through the
// execution's JSON form, by the names the task records give them, so the record is built the same before and after
// the fields land; an old row has them empty.
type execView struct {
	tasks.Execution
	Verdict       json.RawMessage
	Origin        tasks.Origin
	Skill         tasks.Skill
	ResolvedModel string
	// Replies maps an intervention to its reply's judgement id and replier.
	Replies map[string]reply
}

type reply struct{ JudgementID, ReplierID string }

// viewOf is e as an execView.
func viewOf(e tasks.Execution) execView {
	v := execView{Execution: e, Replies: map[string]reply{}}
	for _, iv := range e.Interventions {
		v.Replies[iv.ID] = reply{JudgementID: iv.JudgementID, ReplierID: iv.ReplierID}
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return v
	}
	var extra struct {
		Verdict       json.RawMessage `json:"verdict"`
		Origin        tasks.Origin    `json:"origin"`
		Skill         tasks.Skill     `json:"skill"`
		ResolvedModel string          `json:"resolved_model"`
	}
	if json.Unmarshal(raw, &extra) != nil {
		return v
	}
	if len(extra.Verdict) > 0 && !bytes.Equal(bytes.TrimSpace(extra.Verdict), []byte("null")) {
		v.Verdict = extra.Verdict
	}
	v.Origin, v.Skill, v.ResolvedModel = extra.Origin, extra.Skill, extra.ResolvedModel
	return v
}

// runInput is everything one trajectory is assembled from.
type runInput struct {
	detail     tasks.Detail
	view       execView
	judgements []JudgementRow
	calls      []CallRow
	bundle     *Bundle
	blobs      Blobs
}

// parsed is what the bundle's files hold.
type parsed struct {
	meta      recMeta
	events    []recEvent
	trace     []traceLine
	ledger    []ledgerRec
	manifest  runManifest
	problems  []string // a part that was there and could not be parsed
	partFiles map[string][]File
}

// assemble builds the document from its inputs. It never fails: what it could not read is said in integrity.missing.
func assemble(in runInput) Trajectory {
	e := in.view
	t := Trajectory{
		Schema: Schema, RunID: e.ID, TaskID: e.TaskID, Attempt: e.Attempt,
		Worker:     Worker{ID: e.WorkerID, Name: in.detail.Worker.Name},
		Skill:      Skill{Name: e.Skill.Name, Version: e.Skill.Version},
		Request:    Request{Text: in.detail.Request, SHA256: sha256Hex([]byte(in.detail.Request)), Files: []RequestFile{}},
		StartedAt:  rfc3339(e.StartedAt),
		EndedAt:    rfc3339(e.EndedAt),
		Status:     e.Status,
		Outcome:    e.Outcome.State,
		Verdict:    json.RawMessage("null"),
		Models:     []Model{},
		Steps:      []Step{},
		Calls:      []Call{},
		Questions:  []Question{},
		Judgements: []Judgement{},
		Ledger:     []LedgerRecord{},
		Labels:     Labels{PersonVerdict: in.detail.Verdict, Eval: json.RawMessage("null")},
		Integrity:  Integrity{Parts: map[string]string{}, Missing: []string{}, WorkerWritten: []string{}},
	}
	if len(e.Verdict) > 0 {
		t.Verdict = e.Verdict
	}
	t.Origin = originOf(e.Origin)

	own := ownJudgements(in.judgements, e.ID)
	byID := map[string]JudgementRow{}
	for _, j := range own {
		byID[j.ID] = j
		t.Judgements = append(t.Judgements, Judgement{ID: j.ID, Kind: j.Kind, Actor: actorOf(j.Actor),
			At: rfc3339(j.At), DerivedFrom: j.DerivedFrom, Payload: j.Payload})
	}
	t.Questions = questionsOf(e, byID)

	var p parsed
	if in.bundle == nil {
		t.Integrity.Missing = append(t.Integrity.Missing, "no bundle: nothing was pulled off the desktop for this run")
	} else {
		p = parseBundle(*in.bundle, in.blobs)
		t.Integrity.BundleSHA256 = in.bundle.ManifestSHA
		t.Integrity.Missing = append(t.Integrity.Missing, in.bundle.Missing...)
		t.Integrity.Missing = append(t.Integrity.Missing, p.problems...)
		for _, name := range []string{"recording", "notes", "ledger", "files", "outputs", "run", "soul_trace"} {
			files := p.partFiles[name]
			if len(files) == 0 {
				continue
			}
			label := name
			if name == "run" {
				label = "manifest"
			}
			if len(files) == 1 && name != "recording" && name != "run" {
				t.Integrity.Parts[label] = "sha256:" + files[0].SHA256
			} else {
				t.Integrity.Parts[label] = "sha256:" + ManifestSHA(files)
			}
			// O-1: until the evidence store sits under the gate user, every part is the worker's own to write.
			t.Integrity.WorkerWritten = append(t.Integrity.WorkerWritten, label)
		}
		t.Worker.Harness, t.Worker.Brain = p.manifest.Harness, p.manifest.Brain
		t.Worker.AgentSHA, t.Worker.SoulSHA, t.Worker.RulesSHA = p.manifest.AgentSHA, p.manifest.SoulSHA, p.manifest.RulesSHA
	}

	t.Calls = callsOf(in.calls, p.trace)
	t.Models, t.Usage = modelsOf(t.Calls)
	t.Ledger = ledgerOf(p.ledger)
	t.Steps = stepsOf(p, t.Calls, e, byID)
	labelSteps(t.Steps, t.Ledger, t.Verdict, own)
	return t
}

// ownJudgements is the judgements of this execution, and the task's own ones (a verdict on the task names no
// execution), oldest first.
func ownJudgements(all []JudgementRow, execID string) []JudgementRow {
	var out []JudgementRow
	for _, j := range all {
		if j.ExecutionID == execID || j.ExecutionID == "" {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out
}

func originOf(o tasks.Origin) Origin {
	kind := o.Kind
	if kind == "" {
		kind = tasks.OriginUnknown
	}
	out := Origin{Kind: kind, Ref: o.Ref}
	if o.ActorID != "" {
		out.Actor = &Actor{ID: o.ActorID, Roles: []string{}, Kind: tasks.ActorPerson}
	}
	return out
}

func actorOf(a tasks.Actor) Actor {
	roles := a.Roles
	if roles == nil {
		roles = []string{}
	}
	return Actor{ID: a.ID, Name: a.Name, Roles: roles, Kind: a.Kind}
}

// questionsOf is every time the execution stopped for a person, with the answer and its author.
func questionsOf(e execView, byID map[string]JudgementRow) []Question {
	out := []Question{}
	for _, iv := range e.Interventions {
		q := Question{InterventionID: iv.ID, Kind: iv.Kind, AskedAt: rfc3339(iv.RequestedAt), Text: iv.Request,
			Secret: iv.Secret}
		if !iv.RepliedAt.IsZero() {
			r := e.Replies[iv.ID]
			a := &Answer{JudgementID: r.JudgementID, At: rfc3339(iv.RepliedAt), Text: iv.Reply}
			if j, ok := byID[r.JudgementID]; ok && r.JudgementID != "" {
				actor := actorOf(j.Actor)
				a.Actor = &actor
			}
			q.Answer = a
		}
		out = append(out, q)
	}
	return out
}

// callsOf is the run's calls: the proxy's rows in order; with none (a brain the proxy never sees), soul's trace's call
// lines, which carry no bodies.
func callsOf(rows []CallRow, trace []traceLine) []Call {
	out := []Call{}
	for _, r := range rows {
		c := Call{I: len(out), ID: r.ID, At: rfc3339(r.At), Source: "proxy", Provider: r.Provider,
			Deployment: r.Deployment, Purpose: r.Purpose, Turn: r.Turn, Caller: r.Caller, Status: r.Status, Ms: r.MS,
			PromptSHA256: r.PromptSHA, PrefixSHA256: r.PrefixSHA, ResponseSHA: r.ResponseSHA, Messages: len(r.Messages),
			Usage: Usage{Calls: 1, InputTokens: deref(r.InputTokens), CachedInputTokens: deref(r.CachedInputTokens),
				OutputTokens: deref(r.OutputTokens)}, Problems: r.Problems}
		if c.Problems == nil {
			c.Problems = []string{}
		}
		out = append(out, c)
	}
	if len(out) > 0 {
		return out
	}
	for _, l := range trace {
		if l.Kind != "call" {
			continue
		}
		c := Call{I: len(out), At: l.At, Source: "soul_trace", Provider: l.Provider, Deployment: l.Model,
			Purpose: l.Purpose, Turn: l.Turn, Caller: "soul", Ms: l.Ms, PromptSHA256: l.PromptSHA256,
			Messages: l.Messages, Problems: []string{}}
		if l.Usage != nil {
			c.Usage = Usage{Calls: 1, InputTokens: int64(l.Usage.In), CachedInputTokens: int64(l.Usage.Cached),
				OutputTokens: int64(l.Usage.Out)}
		}
		if l.Failed {
			c.Problems = append(c.Problems, "failed")
		}
		out = append(out, c)
	}
	return out
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// modelsOf is each deployment that served the calls, in order of first use, and the run's usage.
func modelsOf(calls []Call) ([]Model, Usage) {
	models := []Model{}
	index := map[string]int{}
	var u Usage
	for _, c := range calls {
		key := c.Provider + "\x00" + c.Deployment
		i, ok := index[key]
		if !ok {
			i = len(models)
			index[key] = i
			models = append(models, Model{Provider: c.Provider, Deployment: c.Deployment, Purposes: map[string]int{}})
		}
		models[i].Calls++
		models[i].Purposes[c.Purpose]++
		u.InputTokens += c.Usage.InputTokens
		u.CachedInputTokens += c.Usage.CachedInputTokens
		u.OutputTokens += c.Usage.OutputTokens
		u.Calls++
	}
	return models, u
}

// ---------------------------------------------------------------- the bundle's files

// recMeta is the recording's meta.json, as far as the record reads it.
type recMeta struct {
	StartedAt float64 `json:"started_at"`
}

// recEvent is one line of the recording's events.jsonl (docs/recordings.md).
type recEvent struct {
	I        int             `json:"i"`
	T        float64         `json:"t"`
	At       string          `json:"at"`
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Element  json.RawMessage `json:"element"`
	Target   json.RawMessage `json:"target"`
	Text     *string         `json:"text"`
	Withheld json.RawMessage `json:"withheld"`
	Backend  string          `json:"backend"`
	Outcome  string          `json:"outcome"`
	Actor    string          `json:"actor"`
	Before   string          `json:"before"`
	After    string          `json:"after"`
	Change   json.RawMessage `json:"change"`
	Expect   json.RawMessage `json:"expect"`
	Window   string          `json:"window"`
	URL      string          `json:"url"`
	Obs      string          `json:"obs"`
	Turn     string          `json:"turn"`

	at time.Time
}

// traceLine is one line of soul's trace (decurix.soul-turn/v1).
type traceLine struct {
	At           string `json:"at"`
	Turn         string `json:"turn"`
	Kind         string `json:"kind"`
	Purpose      string `json:"purpose"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	PromptSHA256 string `json:"prompt_sha256"`
	Messages     int    `json:"messages"`
	Usage        *struct {
		In, Cached, Out int
	} `json:"usage"`
	Ms      int64     `json:"ms"`
	Failed  bool      `json:"failed"`
	Command *traceCmd `json:"command"`

	at time.Time
}

// traceCmd is a trace line's command.
type traceCmd struct {
	Text         string `json:"text"`
	Guard        string `json:"guard"`
	Exit         *int   `json:"exit"`
	Ms           int64  `json:"ms"`
	OutputSHA256 string `json:"output_sha256"`
}

// ledgerRec is one record of the ledger (<task>.records.json, computer/records.py).
type ledgerRec struct {
	Key      string                 `json:"key"`
	Source   string                 `json:"source"`
	Fields   map[string]ledgerField `json:"fields"`
	SavedAt  *float64               `json:"saved_at"`
	Readback struct {
		Seen   []string `json:"seen"`
		Absent []string `json:"absent"`
	} `json:"readback"`
	order []string // the fields' labels as the file lists them
}

type ledgerField struct {
	Intent  json.RawMessage `json:"intent"`
	Entered json.RawMessage `json:"entered"`
}

// runManifest is the run folder's run.json (decurix.run-manifest/v1).
type runManifest struct {
	Harness  string `json:"harness"`
	Brain    string `json:"brain"`
	AgentSHA string `json:"agent_sha"`
	SoulSHA  string `json:"soul_sha"`
	RulesSHA string `json:"rules_sha"`
}

// partOf names the part a bundle file belongs to (partsOf), or "".
func partOf(task, rel string) string {
	for _, pt := range partsOf(task) {
		if rel == pt.rel || strings.HasPrefix(rel, pt.rel+"/") {
			return pt.name
		}
	}
	return ""
}

// parseBundle reads the bundle's recording, trace, ledger and manifest out of the blob store.
func parseBundle(b Bundle, blobs Blobs) parsed {
	p := parsed{partFiles: map[string][]File{}}
	byPath := map[string]File{}
	for _, f := range b.Files {
		byPath[f.Path] = f
		if name := partOf(b.TaskID, f.Path); name != "" {
			p.partFiles[name] = append(p.partFiles[name], f)
		}
	}
	read := func(rel string) ([]byte, bool) {
		f, ok := byPath[rel]
		if !ok {
			return nil, false
		}
		rc, err := blobs.Open(f.SHA256)
		if err != nil {
			p.problems = append(p.problems, rel+": in the bundle but its blob could not be opened")
			return nil, false
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, maxPartRead))
		if err != nil {
			p.problems = append(p.problems, rel+": could not be read from the blob store")
			return nil, false
		}
		return data, true
	}
	rec := ".decurix/recordings/task-" + b.TaskID
	if data, ok := read(rec + "/meta.json"); ok {
		if json.Unmarshal(data, &p.meta) != nil {
			p.problems = append(p.problems, "recording: meta.json is not JSON")
		}
	}
	if data, ok := read(rec + "/events.jsonl"); ok {
		bad := 0
		eachLine(data, func(line []byte) {
			var ev recEvent
			if json.Unmarshal(line, &ev) != nil {
				bad++
				return
			}
			ev.at = eventTime(ev, p.meta)
			p.events = append(p.events, ev)
		})
		if bad > 0 {
			p.problems = append(p.problems, fmt.Sprintf("recording: %d line(s) of events.jsonl are not JSON", bad))
		}
	}
	if data, ok := read(".decurix/ai/traces/" + b.TaskID + ".jsonl"); ok {
		bad := 0
		eachLine(data, func(line []byte) {
			var l traceLine
			if json.Unmarshal(line, &l) != nil {
				bad++
				return
			}
			if l.Kind == "truncated" {
				p.problems = append(p.problems, "soul_trace: cut at its size limit")
				return
			}
			l.at, _ = time.Parse(time.RFC3339Nano, l.At)
			p.trace = append(p.trace, l)
		})
		if bad > 0 {
			p.problems = append(p.problems, fmt.Sprintf("soul_trace: %d line(s) are not JSON", bad))
		}
	}
	if data, ok := read(".decurix/computer/evidence/" + b.TaskID + ".records.json"); ok {
		recs, err := parseLedger(data)
		if err != nil {
			p.problems = append(p.problems, "ledger: records.json is not a list of records")
		}
		p.ledger = recs
	}
	if data, ok := read(".decurix/runs/" + b.TaskID + "/run.json"); ok {
		if json.Unmarshal(data, &p.manifest) != nil {
			p.problems = append(p.problems, "run: run.json is not JSON")
		}
	}
	return p
}

// eachLine calls fn with every non-blank line of data.
func eachLine(data []byte, fn func([]byte)) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			fn(line)
		}
	}
}

// eventTime is when an event happened: its own "at", else the recording's start plus its "t".
func eventTime(ev recEvent, meta recMeta) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, ev.At); err == nil {
			return t.UTC()
		}
	}
	if meta.StartedAt > 0 {
		return time.Unix(0, int64((meta.StartedAt+ev.T)*1e9)).UTC()
	}
	return time.Time{}
}

// parseLedger reads the ledger's records, keeping each record's fields in the file's order.
func parseLedger(data []byte) ([]ledgerRec, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	var out []ledgerRec
	for _, raw := range raws {
		var r ledgerRec
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		var shape struct {
			Fields json.RawMessage `json:"fields"`
		}
		_ = json.Unmarshal(raw, &shape)
		r.order = objectKeys(shape.Fields)
		out = append(out, r)
	}
	return out, nil
}

// objectKeys is a JSON object's keys in the order written.
func objectKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		key, _ := tok.(string)
		keys = append(keys, key)
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}

// ---------------------------------------------------------------- the ledger

// Ledger and field statuses.
const (
	ledgerMatched  = "matched"  // saved, and every intended value was read back
	ledgerDiffers  = "differs"  // saved, and the page did not show an intended value
	ledgerSaved    = "saved"    // saved, not read back
	ledgerDeclared = "declared" // declared and never saved
	fieldUnchecked = "unchecked"
)

// ledgerOf is the ledger as the document has it. "shown" is "" in v1: the ledger keeps whether each intended value
// was seen on the page after the save, not what was shown.
func ledgerOf(recs []ledgerRec) []LedgerRecord {
	out := []LedgerRecord{}
	for _, r := range recs {
		seen, absent := setOf(r.Readback.Seen), setOf(r.Readback.Absent)
		lr := LedgerRecord{Key: r.Key, Source: r.Source, Fields: []LedgerField{}}
		for _, label := range r.order {
			f := r.Fields[label]
			intended := jsonText(f.Intent)
			if intended == "" {
				continue
			}
			status := fieldUnchecked
			switch {
			case seen[label]:
				status = ledgerMatched
			case absent[label]:
				status = ledgerDiffers
			}
			lr.Fields = append(lr.Fields, LedgerField{Label: label, Intended: intended, AtSave: jsonText(f.Entered),
				Status: status})
		}
		switch {
		case r.SavedAt == nil:
			lr.Status = ledgerDeclared
		case len(r.Readback.Absent) > 0:
			lr.Status = ledgerDiffers
		case len(r.Readback.Seen) > 0:
			lr.Status = ledgerMatched
		default:
			lr.Status = ledgerSaved
		}
		out = append(out, lr)
	}
	return out
}

func setOf(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// jsonText is a JSON value as text: a string as itself, a number or other value as written, null as "".
func jsonText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// ---------------------------------------------------------------- steps

// Gate decisions on a step.
const (
	gateNotNeeded = "not_needed"
	gateAsked     = "asked"
	gateGranted   = "granted"
	gateDenied    = "denied"
	gateRefused   = "refused"
)

// guiApprovalExit is the exit code of a gui command the approval gate held (docs/computer-use.md).
const guiApprovalExit = 3

// commandSlack widens a command's run when an event is matched to it: events carry whole seconds.
const commandSlack = 2 * time.Second

// stepsOf is the run's steps: one per recording event, in order, each with the soul command of its turn that ran it;
// a command that left no event is a step of its own, with no action.
func stepsOf(p parsed, calls []Call, e execView, byID map[string]JudgementRow) []Step {
	type cmd struct {
		line       traceLine
		start, end time.Time
		attached   bool
	}
	var cmds []*cmd
	byTurn := map[string][]*cmd{}
	for _, l := range p.trace {
		if l.Kind != "command" || l.Command == nil {
			continue
		}
		c := &cmd{line: l, end: l.at, start: l.at.Add(-time.Duration(l.Command.Ms) * time.Millisecond)}
		cmds = append(cmds, c)
		byTurn[l.Turn] = append(byTurn[l.Turn], c)
	}

	type draft struct {
		step Step
		at   time.Time
	}
	var drafts []draft
	for _, ev := range p.events {
		s := Step{At: rfc3339(ev.at), Turn: ev.Turn, LedgerDelta: []LedgerDiff{}}
		s.Observation = &Observation{Frame: path.Base(ev.Before), AX: ev.Obs, Window: ev.Window, URL: ev.URL}
		if ev.Before == "" {
			s.Observation.Frame = ""
		}
		target := ev.Element
		if len(bytes.TrimSpace(target)) == 0 || bytes.Equal(bytes.TrimSpace(target), []byte("null")) {
			target = ev.Target
		}
		s.Action = &Action{RecordID: ev.ID, Kind: ev.Kind, Target: nullIfEmpty(target), Text: ev.Text,
			Withheld: nullIfEmpty(ev.Withheld), Backend: ev.Backend, Outcome: ev.Outcome, Actor: ev.Actor}
		s.Diff = &Diff{Before: baseOrEmpty(ev.Before), After: baseOrEmpty(ev.After), Change: jsonText(ev.Change),
			Expect: nullIfEmpty(ev.Expect)}
		// The command of this turn that ran it: the one whose run holds the event's time, else the turn's first.
		if ev.Turn != "" {
			var pick *cmd
			for _, c := range byTurn[ev.Turn] {
				if !ev.at.Before(c.start.Add(-commandSlack)) && !ev.at.After(c.end.Add(commandSlack)) {
					pick = c
					break
				}
			}
			if pick == nil && len(byTurn[ev.Turn]) > 0 {
				pick = byTurn[ev.Turn][0]
			}
			if pick != nil && !pick.attached {
				pick.attached = true
				s.Command = commandOf(pick.line)
			}
		}
		s.Gate = gateOf(ev.Outcome, nil, ev.at, e, byID)
		drafts = append(drafts, draft{step: s, at: ev.at})
	}
	for _, c := range cmds {
		if c.attached {
			continue
		}
		s := Step{At: rfc3339(c.start), Turn: c.line.Turn, Command: commandOf(c.line), LedgerDelta: []LedgerDiff{}}
		s.Gate = gateOf("", c.line.Command, c.start, e, byID)
		drafts = append(drafts, draft{step: s, at: c.start})
	}
	sort.SliceStable(drafts, func(a, b int) bool {
		if !drafts[a].at.Equal(drafts[b].at) {
			return drafts[a].at.Before(drafts[b].at)
		}
		return drafts[a].step.Turn < drafts[b].step.Turn
	})

	out := make([]Step, 0, len(drafts))
	for i, d := range drafts {
		s := d.step
		s.I = i
		if ci := callFor(calls, s.Turn, d.at); ci >= 0 {
			idx := ci
			s.Call = &idx
			c := calls[ci]
			s.Model = &StepModel{Provider: c.Provider, Deployment: c.Deployment}
			s.PromptSHA256, s.PrefixSHA256 = c.PromptSHA256, c.PrefixSHA256
		}
		out = append(out, s)
	}
	return out
}

// callFor is the index of the last call of turn at or before t, or -1 (no turn, or no such call).
func callFor(calls []Call, turn string, t time.Time) int {
	if turn == "" {
		return -1
	}
	found := -1
	for i, c := range calls {
		if c.Turn != turn {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, c.At)
		if err != nil || at.After(t) {
			continue
		}
		found = i
	}
	return found
}

func commandOf(l traceLine) *Command {
	c := l.Command
	tool := ""
	if fields := strings.Fields(c.Text); len(fields) > 0 {
		tool = fields[0]
	}
	return &Command{Text: c.Text, Tool: tool, Exit: c.Exit, Ms: c.Ms, OutputSHA256: c.OutputSHA256, Guard: c.Guard}
}

// gateOf is what the approval gate decided about a step. An event that paused for approval, or a command soul's guard
// asked about, was asked; the answer is the next approval after it, from its reply's judgement (payload.decision).
// A command that ran after being asked was granted, unless the gate held what it did. An event or command the gate
// refused outright is refused.
func gateOf(outcome string, c *traceCmd, at time.Time, e execView, byID map[string]JudgementRow) Gate {
	g := Gate{Decision: gateNotNeeded}
	asked := outcome == "needs_approval"
	if c != nil {
		switch c.Guard {
		case "refused":
			g.Decision = gateRefused
			return g
		case "asked":
			// It ran after a yes, unless what ran is a gui command the approval gate held (the gui contract's exit 3).
			if c.Exit != nil && *c.Exit != guiApprovalExit {
				g.Decision = gateGranted
				return g
			}
			asked = true
		}
	}
	if outcome == "refused" {
		g.Decision = gateRefused
		return g
	}
	if !asked {
		return g
	}
	g.Decision = gateAsked
	for _, iv := range e.Interventions {
		if iv.Kind != tasks.InterventionApproval || iv.RequestedAt.Before(at.Add(-commandSlack)) {
			continue
		}
		r := e.Replies[iv.ID]
		g.RequestID = iv.ID
		if j, ok := byID[r.JudgementID]; ok && r.JudgementID != "" {
			g.JudgementID = j.ID
			var payload struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal(j.Payload, &payload)
			switch payload.Decision {
			case "yes":
				g.Decision = gateGranted
			case "no":
				g.Decision = gateDenied
			}
		}
		break
	}
	return g
}

func nullIfEmpty(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func baseOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	return path.Base(p)
}

// ---------------------------------------------------------------- labels

// Step labels.
const (
	labelOK         = "ok"
	labelCorrected  = "corrected"
	labelOverridden = "overridden"
	labelUnlabelled = "unlabelled"
)

// independentBasis are the kinds of check that make a step "ok" when the run's verdict is done (W0.5: a check by code,
// the ledger read back, or a person; a model or the screen alone does not).
var independentBasis = map[string]bool{"code": true, "ledger": true, "person": true}

// labelSteps labels every step (docs/trajectory.md, "Step labels"):
//   - overridden: a person acted on the screen (the event's actor is human), or the approval for it was denied;
//   - corrected: a correction judgement on the run names the step's record or field, or the step's ledger field ended
//     "differs" and a person answered after it;
//   - ok: neither, and the run's verdict is done on a check by code, the ledger or a person;
//   - unlabelled: everything else.
func labelSteps(steps []Step, ledger []LedgerRecord, verdict json.RawMessage, judgements []JudgementRow) {
	var v struct {
		Verdict string   `json:"verdict"`
		Basis   []string `json:"basis"`
	}
	_ = json.Unmarshal(verdict, &v)
	doneIndependently := false
	if v.Verdict == tasks.GateDone {
		for _, b := range v.Basis {
			doneIndependently = doneIndependently || independentBasis[b]
		}
	}
	var corrections, answers []JudgementRow
	for _, j := range judgements {
		switch {
		case j.Kind == tasks.JudgementCorrection:
			corrections = append(corrections, j)
		case j.Kind == tasks.JudgementAnswer && j.Actor.Kind == tasks.ActorPerson:
			answers = append(answers, j)
		}
	}
	for i := range steps {
		s := &steps[i]
		switch {
		case s.Action != nil && s.Action.Actor == "human", s.Gate.Decision == gateDenied:
			s.Label = labelOverridden
		case corrected(s, ledger, corrections, answers):
			s.Label = labelCorrected
		case doneIndependently:
			s.Label = labelOK
		default:
			s.Label = labelUnlabelled
		}
	}
}

// corrected reports whether a step's record or field was corrected by a person (labelSteps).
func corrected(s *Step, ledger []LedgerRecord, corrections, answers []JudgementRow) bool {
	label := targetName(s)
	if label == "" {
		return false
	}
	at, _ := time.Parse(time.RFC3339Nano, s.At)
	for _, rec := range ledger {
		for _, f := range rec.Fields {
			if !strings.EqualFold(strings.TrimSpace(f.Label), label) {
				continue
			}
			for _, c := range corrections {
				text := strings.ToLower(c.Text)
				if strings.Contains(text, strings.ToLower(rec.Key)) || strings.Contains(text, strings.ToLower(f.Label)) {
					return true
				}
			}
			if f.Status == ledgerDiffers {
				for _, a := range answers {
					if a.At.After(at) {
						return true
					}
				}
			}
		}
	}
	return false
}

// targetName is the name of the element a step acted on, or "".
func targetName(s *Step) string {
	if s.Action == nil {
		return ""
	}
	var el struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(s.Action.Target, &el) == nil {
		return strings.TrimSpace(el.Name)
	}
	return ""
}

// ---------------------------------------------------------------- small helpers

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
