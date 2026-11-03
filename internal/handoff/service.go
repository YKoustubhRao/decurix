package handoff

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// File is one file carried from the sending worker to the receiving one.
type File struct {
	Name string
	Data []byte
}

// Submitter sends a request to one worker as a tracked task, after writing
// files (if any) into dir on that worker, and returns the task ID. It must
// refuse a worker that is busy with an error carrying a DesktopBusy() string
// method (httpapi's *DesktopBusyError): that keeps the handoff queued, not
// failed. Backed by *httpapi.Server's SubmitTracked / SubmitTrackedWithFiles.
type Submitter interface {
	SubmitHandoff(ctx context.Context, workerID, content, dir string, files []File) (taskID string, err error)
}

// FileReader reads files (absolute paths under the worker's home) out of a
// worker. Backed by desktop.Manager.CopyOut and ReadTarFiles.
type FileReader interface {
	ReadFiles(ctx context.Context, workerID string, paths []string) ([]File, error)
}

// FileWriter writes files into dir on a worker. Backed by
// desktop.Manager.CopyFiles. Optional: without it the result's files stay
// on the receiving worker and are named there.
type FileWriter interface {
	WriteFiles(ctx context.Context, workerID, dir string, files []File) error
}

// TaskOutcome is what the task records say about the receiving task.
type TaskOutcome struct {
	// Found is false while the task is not on record yet.
	Found bool
	// Status is the task's status (internal/tasks Status* values).
	Status string
	// Report is the final reply of the task's latest attempt.
	Report string
	// Checked is whether the latest attempt is the work done and checked (nil: not said): false for a stop at a
	// person's no, whatever its own check said.
	Checked *bool
	// Evidence is what the receiver's final check said it saw (its verdict's evidence and passing checks).
	Evidence []string
	// OpenQuestions are the receiver's questions to a person not answered yet.
	OpenQuestions []string
}

// Tasks is what this package needs from internal/tasks.Store: the outcome of
// a receiving task, and whether a worker is free for a new one.
type Tasks interface {
	TaskOutcome(taskID string) (TaskOutcome, error)
	// Busy says why a worker cannot take a new task now, or "" when it can.
	Busy(workerID string) (string, error)
}

// Workers resolves a worker ID a caller gave to the canonical ID of a worker
// that exists. Backed by desktop.SanitizeID plus the desktop manager's Get.
type Workers interface {
	Resolve(ctx context.Context, raw string) (id string, ok bool)
}

// WorkerFinder lists the workers that hold a role or a skill, best first.
// Exactly one of role and skill is non-empty. Backed by the skill registry;
// this package does not import it.
type WorkerFinder interface {
	Holders(ctx context.Context, role, skill string) ([]string, error)
}

// Limits on what one handoff may carry.
const (
	MaxRequestBytes = 12000
	MaxInputs       = 40
	MaxInputKey     = 100
	MaxInputValue   = 4000
	MaxFiles        = 20
	MaxExpected     = 2000
	MaxQuestion     = 2000
	// MaxDepth bounds a chain of handoffs (a receiver handing on in turn).
	MaxDepth = 3
)

// Default times; a zero field on Service uses these.
const (
	defaultQueueTimeout   = 30 * time.Minute
	defaultAskTimeout     = 15 * time.Minute
	// defaultDelegateTimeout bounds a handed-on piece of work: a receiver that never reports (its desktop gone, a
	// question nobody answers) must not keep the sender waiting for good (C-07).
	defaultDelegateTimeout = 24 * time.Hour
	// sendingLease is how long a handoff may stay "sending" before it is taken as a send the control plane never
	// finished (it stopped mid-send): failed with a reason, never sent a second time.
	sendingLease = 15 * time.Minute
	defaultRecordGrace    = 10 * time.Minute
	defaultTickInterval   = 15 * time.Second
	defaultAskWaitCeiling = 60 * time.Second
)

// home is the receiving worker's home; files are carried under it.
const home = "/home/headless"

// InboxDir is where a handoff's files land on the receiving worker.
func InboxDir(id string) string { return home + "/handoffs/" + id }

// ReturnDir is where a result's files land back on the sending worker.
func ReturnDir(id string) string { return home + "/handoffs/" + id + "/returned" }

// OutDir is where the receiving worker is told to save the files its result hands back. Only files there go back to
// the sender, and only with a verified result: never a file a report merely names (review 2026-11-03, C-06).
func OutDir(id string) string { return InboxDir(id) + "/out" }

// InputError is a request refused for what it asked; its message is chosen
// by this package and safe to show the caller.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func refuse(format string, a ...any) error { return &InputError{Msg: fmt.Sprintf(format, a...)} }

// Service runs handoffs: creates them, sends them to a receiving worker,
// and turns the receiving task's outcome into a result.
type Service struct {
	Store   *Store
	Submit  Submitter
	Tasks   Tasks
	Workers Workers
	Finder  WorkerFinder // nil: only handoffs to a named worker
	Reader  FileReader   // nil: handoffs that carry files are refused
	Writer  FileWriter   // nil: result files are named where they are

	QueueTimeout    time.Duration // a handoff no worker took in this long fails
	AskTimeout      time.Duration // a question not answered in this long fails
	DelegateTimeout time.Duration // handed-on work not finished in this long fails
	RecordGrace  time.Duration // a sent task with no record after this long fails
	Interval     time.Duration // how often Run advances open handoffs

	// mu makes each read-then-write of a handoff one step: the runner, a
	// status read and a cancel never advance the same row at once. It is let go
	// while a request is submitted (the row is claimed as sending first), since
	// waking a stopped desktop can take minutes.
	mu  sync.Mutex
	now func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// DelegateRequest is a new handoff of work as a worker asked for it.
type DelegateRequest struct {
	FromWorker string
	FromTask   string
	ToWorker   string
	ToRole     string
	ToSkill    string
	Request    string
	Inputs     map[string]string
	Files      []string
	Expected   string
}

// AskRequest is a question a worker puts to another worker taught a skill.
type AskRequest struct {
	FromWorker string
	FromTask   string
	Skill      string
	Question   string
}

// Delegate validates and stores a handoff of work, then tries to send it at
// once; a busy target leaves it queued for Run to send later.
func (s *Service) Delegate(ctx context.Context, req DelegateRequest) (Handoff, error) {
	from, ok := s.Workers.Resolve(ctx, req.FromWorker)
	if !ok {
		return Handoff{}, refuse("the sending worker is not known")
	}

	h := Handoff{
		Kind: KindDelegate, FromWorker: from, FromTask: strings.TrimSpace(req.FromTask),
		ToRole: strings.TrimSpace(req.ToRole), ToSkill: strings.TrimSpace(req.ToSkill),
		Request: strings.TrimSpace(req.Request), Expected: strings.TrimSpace(req.Expected),
		Inputs: map[string]string{}, Files: []string{},
	}

	targets := 0
	for _, t := range []string{req.ToWorker, h.ToRole, h.ToSkill} {
		if strings.TrimSpace(t) != "" {
			targets++
		}
	}
	if targets != 1 {
		return Handoff{}, refuse("name exactly one of to_worker, to_role and to_skill")
	}
	if h.Request == "" {
		return Handoff{}, refuse("request is required")
	}
	if len(h.Request) > MaxRequestBytes {
		return Handoff{}, refuse("request is longer than %d bytes", MaxRequestBytes)
	}
	if len(h.Expected) > MaxExpected {
		return Handoff{}, refuse("expected is longer than %d bytes", MaxExpected)
	}
	if len(req.Inputs) > MaxInputs {
		return Handoff{}, refuse("at most %d inputs", MaxInputs)
	}
	for k, v := range req.Inputs {
		k = strings.TrimSpace(k)
		if k == "" || len(k) > MaxInputKey || strings.ContainsAny(k, "\r\n") {
			return Handoff{}, refuse("an input name is empty, too long or spans lines")
		}
		if len(v) > MaxInputValue {
			return Handoff{}, refuse("input %q is longer than %d bytes", k, MaxInputValue)
		}
		h.Inputs[k] = v
	}
	if len(req.Files) > MaxFiles {
		return Handoff{}, refuse("at most %d files", MaxFiles)
	}
	for _, f := range req.Files {
		p, ok := homePath(f)
		if !ok {
			return Handoff{}, refuse("file %q is not a path under %s", f, home)
		}
		h.Files = append(h.Files, p)
	}
	if len(h.Files) > 0 && s.Reader == nil {
		return Handoff{}, refuse("this control plane cannot carry files between workers")
	}

	if req.ToWorker != "" {
		to, ok := s.Workers.Resolve(ctx, req.ToWorker)
		if !ok {
			return Handoff{}, refuse("the receiving worker is not known")
		}
		h.ToWorker = to
	} else if s.Finder == nil {
		return Handoff{}, refuse("this control plane cannot find workers by role or skill; name a worker")
	}

	chain, depth, err := s.chain(h.FromTask)
	if err != nil {
		return Handoff{}, err
	}
	if depth > MaxDepth {
		return Handoff{}, refuse("this work has already been handed on %d times; do it here or ask a person", MaxDepth)
	}
	h.Depth = depth
	chain[from] = true
	if h.ToWorker != "" && chain[h.ToWorker] {
		return Handoff{}, refuse("that worker is waiting on this work already; handing it back would never finish")
	}

	h, err = s.Store.Create(h)
	if err != nil {
		return Handoff{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.advance(ctx, h), nil
}

// Ask stores a question and sends it to a free worker that holds the skill.
// No free holder fails it at once, so the asker goes to a person without waiting.
func (s *Service) Ask(ctx context.Context, req AskRequest) (Handoff, error) {
	from, ok := s.Workers.Resolve(ctx, req.FromWorker)
	if !ok {
		return Handoff{}, refuse("the asking worker is not known")
	}
	skill, question := strings.TrimSpace(req.Skill), strings.TrimSpace(req.Question)
	if skill == "" || question == "" {
		return Handoff{}, refuse("skill and question are required")
	}
	if len(question) > MaxQuestion || len(skill) > MaxInputKey {
		return Handoff{}, refuse("the question or the skill name is too long")
	}
	if s.Finder == nil {
		return Handoff{}, refuse("this control plane cannot find workers by skill")
	}

	chain, depth, err := s.chain(strings.TrimSpace(req.FromTask))
	if err != nil {
		return Handoff{}, err
	}
	if depth > MaxDepth {
		return Handoff{}, refuse("this work has already been handed on %d times; ask a person", MaxDepth)
	}
	chain[from] = true

	h := Handoff{Kind: KindAsk, FromWorker: from, FromTask: strings.TrimSpace(req.FromTask), ToSkill: skill,
		Request: question, Depth: depth}

	to, why := s.pick(ctx, "", skill, chain)
	if to == "" {
		h.Status, h.Reason = StatusFailed, why+" - ask a person instead"
		return s.Store.Create(h)
	}
	h.ToWorker = to

	h, err = s.Store.Create(h)
	if err != nil {
		return Handoff{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.advance(ctx, h), nil
}

// chain walks back from the sending task through the handoffs that brought
// it, returning the workers already waiting on this work and the depth a new
// handoff from it has.
func (s *Service) chain(fromTask string) (map[string]bool, int, error) {
	waiting := map[string]bool{}
	depth := 0
	task := fromTask

	for i := 0; i <= MaxDepth && task != ""; i++ {
		parent, err := s.Store.ByTask(task)
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if depth == 0 {
			depth = parent.Depth + 1
		}
		waiting[parent.FromWorker] = true
		task = parent.FromTask
	}

	return waiting, depth, nil
}

// pick returns a free worker holding role or skill that is not in skip, or
// "" and why there is none.
func (s *Service) pick(ctx context.Context, role, skill string, skip map[string]bool) (string, string) {
	what := "role " + role
	if role == "" {
		what = "skill " + skill
	}

	holders, err := s.Finder.Holders(ctx, role, skill)
	if err != nil {
		log.Printf("handoff: finding holders of %s: %v", what, err)
		return "", "the workers holding " + what + " could not be listed"
	}

	candidates := 0
	for _, raw := range holders {
		id, ok := s.Workers.Resolve(ctx, raw)
		if !ok || skip[id] {
			continue
		}
		candidates++
		why, err := s.Tasks.Busy(id)
		if err == nil && why == "" {
			return id, ""
		}
	}

	if candidates == 0 {
		return "", "no other worker holds " + what
	}
	return "", "every worker holding " + what + " is busy"
}

// Get returns a handoff brought up to date with its receiving task.
func (s *Service) Get(ctx context.Context, id string) (Handoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h, err := s.Store.Get(id)
	if err != nil {
		return Handoff{}, err
	}

	return s.advance(ctx, h), nil
}

// Cancel closes an open handoff. by, when not "", must be the worker that
// sent it. A receiving task already running is not stopped: its result is
// simply no longer collected.
func (s *Service) Cancel(ctx context.Context, id, by string) (Handoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h, err := s.Store.Get(id)
	if err != nil {
		return Handoff{}, err
	}
	if by != "" {
		if who, ok := s.Workers.Resolve(ctx, by); !ok || who != h.FromWorker {
			return Handoff{}, refuse("only the worker that sent a handoff can cancel it")
		}
	}
	if !h.Open() {
		return h, nil
	}

	h.Status, h.Reason = StatusCancelled, "cancelled before a result came back"
	if h.TaskID != "" {
		h.Reason += "; the receiving worker's task " + h.TaskID + " was not stopped"
	}

	return s.save(h), nil
}

// Tick advances every open handoff once.
func (s *Service) Tick(ctx context.Context) {
	open, err := s.Store.OpenHandoffs()
	if err != nil {
		log.Printf("handoff: listing open handoffs: %v", err)
		return
	}

	for _, h := range open {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		// Read again under the lock: a cancel or a status read may have moved it.
		if fresh, err := s.Store.Get(h.ID); err == nil && fresh.Open() {
			s.advance(ctx, fresh)
		}
		s.mu.Unlock()
	}
}

// Run advances open handoffs every Interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(orDefault(s.Interval, defaultTickInterval))
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// save writes h, returning the stored row when it had become final meanwhile.
func (s *Service) save(h Handoff) Handoff {
	saved, ok, err := s.Store.Save(h)
	if err != nil {
		log.Printf("handoff %s: %v", h.ID, err)
		return h
	}
	if !ok {
		if stored, err := s.Store.Get(h.ID); err == nil {
			return stored
		}
	}
	return saved
}

// advance moves one open handoff on: sends a queued one when a worker is
// free, and reads a running one's task. Called with s.mu held.
func (s *Service) advance(ctx context.Context, h Handoff) Handoff {
	switch h.Status {
	case StatusQueued:
		return s.dispatch(ctx, h)
	case StatusRunning:
		return s.collect(ctx, h)
	case StatusSending:
		// Another dispatch is submitting it - or the control plane stopped mid-send, which only the lease can tell.
		if s.clock().Sub(h.UpdatedAt) > sendingLease {
			h.Status = StatusFailed
			h.Reason = "the control plane stopped while sending this request, so whether it reached the receiving worker is unknown - check it before handing it on again"
			return s.save(h)
		}
		return h
	default:
		return h
	}
}

// busyError is how a Submitter says "try later".
type busyError interface{ DesktopBusy() string }

// dispatch sends a queued handoff to its worker (picking one for a role or
// skill), or leaves it queued while every candidate is busy.
func (s *Service) dispatch(ctx context.Context, h Handoff) Handoff {
	now := s.clock()
	if now.Sub(h.CreatedAt) > orDefault(s.QueueTimeout, defaultQueueTimeout) {
		h.Status = StatusFailed
		h.Reason = "no worker took it within " + orDefault(s.QueueTimeout, defaultQueueTimeout).String() + " (" + h.Reason + ")"
		return s.save(h)
	}

	to := h.ToWorker
	explicit := h.Kind == KindDelegate && h.ToRole == "" && h.ToSkill == ""
	if !explicit && (to == "" || h.Kind == KindDelegate) {
		skip, _, err := s.chain(h.FromTask)
		if err != nil {
			log.Printf("handoff %s: %v", h.ID, err)
			return h
		}
		skip[h.FromWorker] = true
		picked, why := s.pick(ctx, h.ToRole, h.ToSkill, skip)
		if picked == "" {
			if strings.HasPrefix(why, "no other worker holds") || h.Kind == KindAsk {
				h.Status, h.Reason = StatusFailed, why
			} else {
				h.Reason = why
			}
			return s.save(h)
		}
		to = picked
	}

	var files []File
	if len(h.Files) > 0 {
		read, err := s.Reader.ReadFiles(ctx, h.FromWorker, h.Files)
		if err != nil {
			log.Printf("handoff %s: reading files from %s: %v", h.ID, h.FromWorker, err)
			h.Status, h.Reason = StatusFailed, "the files to hand over could not be read from the sending worker"
			return s.save(h)
		}
		files = read
	}

	content := composeRequest(h, files)
	dir := ""
	if len(files) > 0 {
		dir = InboxDir(h.ID)
	}

	// Claimed as sending, then submitted without the service's lock: a stopped desktop can take minutes to wake, and
	// every other handoff's status read and the runner would wait behind it (C-16).
	claimed, err := s.Store.Claim(h.ID)
	if err != nil || !claimed {
		if err != nil {
			log.Printf("handoff %s: %v", h.ID, err)
		}
		if fresh, gerr := s.Store.Get(h.ID); gerr == nil {
			return fresh
		}
		return h
	}
	h.Status = StatusSending
	var taskID string
	func() {
		// Re-locked by a defer, so a panicking submit still leaves the lock as the caller's deferred unlock expects
		// (an unlock of an unlocked mutex is fatal, past any recover: RW1-F-09).
		s.mu.Unlock()
		defer s.mu.Lock()
		taskID, err = s.Submit.SubmitHandoff(ctx, to, content, dir, files)
	}()
	h.Status = StatusQueued
	var busy busyError
	if errors.As(err, &busy) {
		h.Reason = "waiting for worker " + to + " to be free: " + busy.DesktopBusy()
		if h.Kind == KindAsk {
			h.Status, h.Reason = StatusFailed, "worker "+to+" became busy before the question reached it - ask a person instead"
		}
		return s.save(h)
	}
	if err != nil {
		log.Printf("handoff %s: submitting to %s: %v", h.ID, to, err)
		h.Status, h.Reason = StatusFailed, "the request could not be sent to worker "+to
		return s.save(h)
	}

	h.ToWorker, h.TaskID, h.DispatchedAt = to, taskID, now
	h.Status, h.Reason = StatusRunning, "worker "+to+" is working on it"
	return s.save(h)
}

// collect reads a running handoff's task and, once it ended, its result.
func (s *Service) collect(ctx context.Context, h Handoff) Handoff {
	now := s.clock()
	out, err := s.Tasks.TaskOutcome(h.TaskID)
	if err != nil {
		log.Printf("handoff %s: reading task %s: %v", h.ID, h.TaskID, err)
		return h
	}

	if !out.Found {
		if now.Sub(h.DispatchedAt) > orDefault(s.RecordGrace, defaultRecordGrace) {
			h.Status, h.Reason = StatusFailed, "the receiving task never appeared in the task records"
			return s.save(h)
		}
		return h
	}

	r := Result{Report: out.Report, OpenQuestions: out.OpenQuestions}

	switch out.Status {
	case tasks.StatusDone:
		r.Evidence = out.Evidence
		if out.Checked != nil && *out.Checked {
			r.Verified = true
			// Only a verified result hands files back, and only those saved in the handoff's out folder.
			r.Files = s.returnFiles(ctx, h)
			h.Status, h.Reason = StatusDone, "worker "+h.ToWorker+"'s final check confirmed it"
		} else {
			h.Status = StatusNotDone
			h.Reason = "worker " + h.ToWorker + " reported back, but its final check did not confirm the result - treat it as not done"
		}
	case tasks.StatusError:
		h.Status, h.Reason = StatusFailed, "worker "+h.ToWorker+" reported that it could not do it"
	case tasks.StatusAbandoned:
		h.Status, h.Reason = StatusFailed, "worker "+h.ToWorker+"'s task was abandoned"
	case tasks.StatusQuery:
		// "query" is an end: the receiver's desktop stopped while it waited on a person (tasks.CloseOpen).
		h.Status, h.Reason = StatusFailed, "worker "+h.ToWorker+"'s task ended while waiting on a person"
	case tasks.StatusWaiting:
		h.Reason = "worker " + h.ToWorker + " asked a person and is waiting for the answer"
	default:
		h.Reason = "worker " + h.ToWorker + " is working on it"
	}

	if h.Open() && h.Kind == KindAsk && now.Sub(h.DispatchedAt) > orDefault(s.AskTimeout, defaultAskTimeout) {
		h.Status, h.Reason = StatusFailed, "worker "+h.ToWorker+" did not answer in time - ask a person instead"
	}
	if limit := orDefault(s.DelegateTimeout, defaultDelegateTimeout); h.Open() && h.Kind == KindDelegate && now.Sub(h.DispatchedAt) > limit {
		h.Status, h.Reason = StatusFailed, "worker "+h.ToWorker+" did not finish within "+limit.String()+" - do it here or ask a person"
	}

	h.Result = r
	return s.save(h)
}

// returnFiles copies the files the receiver saved in the handoff's out folder (OutDir) back to the sending worker and
// returns their paths there. Nothing else on the receiver is read, whatever its report names. With no out folder there
// is nothing to return; without a FileWriter, or when copying fails, the folder on the receiving worker is named.
func (s *Service) returnFiles(ctx context.Context, h Handoff) []string {
	if s.Reader == nil {
		return []string{}
	}
	out := []string{OutDir(h.ID)}
	files, err := s.Reader.ReadFiles(ctx, h.ToWorker, out)
	if err != nil || len(files) == 0 {
		// Most often the receiver made no files: an out folder that is not there.
		if err != nil {
			log.Printf("handoff %s: no files returned from %s: %v", h.ID, OutDir(h.ID), err)
		}
		return []string{}
	}
	if s.Writer == nil {
		return onWorker(h.ToWorker, out)
	}
	if err := s.Writer.WriteFiles(ctx, h.FromWorker, ReturnDir(h.ID), files); err != nil {
		log.Printf("handoff %s: returning files: %v", h.ID, err)
		return onWorker(h.ToWorker, out)
	}

	returned := make([]string, 0, len(files))
	for _, f := range files {
		returned = append(returned, ReturnDir(h.ID)+"/"+path.Base(f.Name))
	}
	return returned
}

func onWorker(worker string, paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = p + " (on worker " + worker + ", not copied back)"
	}
	return out
}

// homePath cleans a path the receiver must get; ok only under the home folder.
func homePath(p string) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if strings.HasPrefix(p, "~/") {
		p = home + p[1:]
	}
	clean := path.Clean(p)
	if !strings.HasPrefix(clean, home+"/") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		return "", false
	}
	// A program's own data - the agent's state, Claude's credentials, keys, a browser profile - lives in a dot-folder
	// of the home; no work does. A report naming one must not have it copied to another worker (review 2026-11-02:
	// ~/.claude/.credentials.json could be handed back).
	for _, part := range strings.Split(strings.TrimPrefix(clean, home+"/"), "/") {
		if strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	return clean, true
}

// composeRequest is the request the receiving worker is sent.
func composeRequest(h Handoff, files []File) string {
	var b strings.Builder

	if h.Kind == KindAsk {
		fmt.Fprintf(&b, "Worker %s asks you a question about %s (handoff %s). Answer only from what you were taught "+
			"about %s - your guides, kept skills and lessons. Do not change anything on the screen, in an application "+
			"or in a file to answer it. If what you were taught does not settle it, answer exactly \"I do not know\".\n\n"+
			"Question: %s", h.FromWorker, h.ToSkill, h.ID, h.ToSkill, h.Request)
		return b.String()
	}

	fmt.Fprintf(&b, "Worker %s handed you this request (handoff %s). Do it as you would any request; your own final "+
		"check decides whether it is done.\n\n%s", h.FromWorker, h.ID, h.Request)

	if len(h.Inputs) > 0 {
		keys := make([]string, 0, len(h.Inputs))
		for k := range h.Inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\n\nInputs:")
		for _, k := range keys {
			fmt.Fprintf(&b, "\n- %s: %s", k, h.Inputs[k])
		}
	}

	if len(files) > 0 {
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = path.Base(f.Name)
		}
		fmt.Fprintf(&b, "\n\nFiles for this request are in %s: %s", InboxDir(h.ID), strings.Join(names, ", "))
	}

	if h.Expected != "" {
		fmt.Fprintf(&b, "\n\nWhen it is done, this must show: %s", h.Expected)
	}

	fmt.Fprintf(&b, "\n\nSave every file you make for this request in %s: only files there are handed back, and only "+
		"once your final check confirms the work.", OutDir(h.ID))
	b.WriteString("\n\nIn your report, say what you did, what you saw that shows it is done, the full paths of any " +
		"files you made, and plainly any part you could not do.")
	return b.String()
}
