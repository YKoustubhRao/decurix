package workqueue

import (
	"context"
	"errors"
	"fmt"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Worker is one desktop that can take work from the queue, with the roles it
// carries and the skills it has installed (from the org's skill registry).
type Worker struct {
	ID     string
	Roles  []string
	Skills []string
}

// WorkerSource lists the workers that exist and says whether one is busy
// with work from anywhere (a person's chat, a schedule, a handoff).
type WorkerSource interface {
	// Workers lists every worker the queue may send to.
	Workers(ctx context.Context) ([]Worker, error)
	// Busy says why a worker cannot take a new task now, "" when it can.
	Busy(worker string) (string, error)
}

// TaskSubmitter sends a request (and its files, written into dir first) to a
// worker through the ordinary task submission path, returning the task ID it
// landed under. A refusal because the worker is busy is an error with a
// DesktopBusy() string method: "try again later", not a failure.
type TaskSubmitter interface {
	Submit(ctx context.Context, worker, request, dir string, files []File) (taskID string, err error)
}

// TaskState is what the task records say about a submitted task.
type TaskState struct {
	Found bool
	// Status is the latest execution's status: queued, running, waiting,
	// done, error, query or abandoned (internal/tasks).
	Status string
	// Checked is whether the desktop's final check judged the final reply
	// (and it was not a stop at a person's refusal); nil when not said.
	Checked *bool
	// Verdict is a person's verdict on the task: "correct", "wrong" or "".
	Verdict string
	Reply   string
}

// TaskStatus reads a submitted task's state.
type TaskStatus interface {
	TaskState(taskID string) (TaskState, error)
}

// Spend is what a worker's tasks have cost.
type Spend struct {
	USD    float64 `json:"usd"`
	Tokens int64   `json:"tokens"`
}

// SpendSource sums a worker's recorded spend since a moment.
type SpendSource interface {
	Spend(worker string, since time.Time) (Spend, error)
}

// Clock tells the dispatcher the time.
type Clock func() time.Time

// Dispatcher hands queued items to workers and follows them to their end.
type Dispatcher struct {
	Store     *Store
	Workers   WorkerSource
	Submitter TaskSubmitter
	Tasks     TaskStatus
	Spend     SpendSource
	Clock     Clock
	// Location is where a day and a month start; nil means time.Local.
	Location *time.Location
	// Backoff is how long an item waits after a failed send; 0 means a minute.
	Backoff time.Duration

	mu sync.Mutex // one Tick at a time
	// lastPrune is when done items' files were last pruned (pruneFiles).
	lastPrune time.Time
	wake      chan struct{}
	once      sync.Once
}

// now is the dispatcher's clock.
func (d *Dispatcher) now() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

// PeriodStart is when the period containing t began, in the dispatcher's location.
func (d *Dispatcher) PeriodStart(period string, t time.Time) time.Time {
	loc := d.Location
	if loc == nil {
		loc = time.Local
	}
	t = t.In(loc)
	if period == PeriodMonth {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// InboxDir is where an item's files are written on its worker's desktop.
func InboxDir(itemID string) string { return "/home/headless/work/inbox/" + itemID }

// Wake asks Run to tick now (after an enqueue), without waiting for the interval.
func (d *Dispatcher) Wake() {
	d.once.Do(func() { d.wake = make(chan struct{}, 1) })
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run ticks every interval until ctx ends.
func (d *Dispatcher) Run(ctx context.Context, every time.Duration) {
	d.once.Do(func() { d.wake = make(chan struct{}, 1) })
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := d.Tick(ctx); err != nil {
			log.Printf("workqueue: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

// tickState is what one Tick has learnt about the workforce.
type tickState struct {
	workers  []Worker
	byID     map[string]Worker
	caps     map[string]Capacity
	limits   []Limit
	inFlight map[string]int
	spend    map[string]string // worker -> "" under budget, else why over
	busy     map[string]string // worker -> why busy, "" free
}

// Tick follows in-flight items to their end, releases budget-held items a
// worker can now afford, and assigns queued items to free eligible workers.
func (d *Dispatcher) Tick(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.follow(); err != nil {
		return err
	}
	d.pruneFiles()

	workers, err := d.Workers.Workers(ctx)
	if err != nil {
		return fmt.Errorf("listing workers: %w", err)
	}
	st := &tickState{workers: workers, byID: map[string]Worker{}, inFlight: map[string]int{},
		spend: map[string]string{}, busy: map[string]string{}}
	sort.Slice(st.workers, func(i, j int) bool { return st.workers[i].ID < st.workers[j].ID })
	for _, w := range st.workers {
		st.byID[w.ID] = w
	}
	if st.caps, err = d.Store.Capacities(); err != nil {
		return err
	}
	if st.limits, err = d.Store.Limits(); err != nil {
		return err
	}
	running, err := d.Store.inStatus(StatusRunning)
	if err != nil {
		return err
	}
	for _, it := range running {
		st.inFlight[it.AssignedWorker]++
	}

	if err := d.releaseHeld(st); err != nil {
		return err
	}
	return d.assign(ctx, st)
}

// doneFilesKept is how long a done item keeps the files it was sent with (up to 25 MiB each, in the database): long
// enough to look at, not for good (C-28). An item that may still be retried keeps its files.
const doneFilesKept = 7 * 24 * time.Hour

// pruneFiles drops the files of items done more than doneFilesKept ago, at most once an hour.
func (d *Dispatcher) pruneFiles() {
	now := d.now()
	if !d.lastPrune.IsZero() && now.Sub(d.lastPrune) < time.Hour {
		return
	}
	d.lastPrune = now
	if n, err := d.Store.PruneFiles(now.Add(-doneFilesKept)); err != nil {
		log.Printf("workqueue: %v", err)
	} else if n > 0 {
		log.Printf("workqueue: dropped %d file(s) of items done more than %s ago", n, doneFilesKept)
	}
}

// follow moves running items forward as their tasks end, and holds any item
// left assigned by a control plane that stopped mid-send (no Tick is running
// now, so an assigned item is one a send may or may not have reached).
func (d *Dispatcher) follow() error {
	items, err := d.Store.inStatus(StatusAssigned, StatusRunning)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Status == StatusAssigned {
			if err := d.Store.hold(it.ID, []string{StatusAssigned}, HoldInterrupted, fmt.Sprintf(
				"interrupted while being sent to %s: check that desktop's chat for it, then retry or cancel", it.AssignedWorker)); err != nil {
				return err
			}
			continue
		}
		state, err := d.Tasks.TaskState(it.TaskID)
		if err != nil {
			log.Printf("workqueue: reading task %s of %s: %v", it.TaskID, it.ID, err)
			continue
		}
		status, reason, ended := outcome(state)
		if !ended {
			if err := d.Store.note(it.ID, StatusRunning, reason); err != nil {
				return err
			}
			continue
		}
		if err := d.Store.finish(it.ID, status, reason); err != nil {
			return err
		}
	}
	return nil
}

// outcome maps a task's state to the item's: done only when the final was
// checked (or a person judged it correct) - no unseen success.
func outcome(s TaskState) (status, reason string, ended bool) {
	if !s.Found {
		return StatusFailed, "the task record is missing", true
	}
	if s.Verdict == "wrong" {
		return StatusNotDone, "a person judged the result wrong", true
	}
	switch s.Status {
	case "queued", "running", "":
		return "", "", false
	case "waiting":
		return "", "waiting on a person in the worker's chat", false
	case "done":
		switch {
		case s.Checked != nil && *s.Checked:
			return StatusDone, "", true
		case s.Verdict == "correct":
			return StatusDone, "judged correct by a person", true
		default:
			return StatusNotDone, "finished without a checked final report: a person should look at it", true
		}
	case "error":
		return StatusFailed, "the worker reported it could not do it: " + truncate(firstLine(s.Reply), 300), true
	case "query", "abandoned":
		return StatusNotDone, "ended while waiting on a person", true
	default:
		return StatusNotDone, "ended as " + s.Status, true
	}
}

// firstLine is s up to its first newline.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// candidates are the workers an item may go to, ignoring load and budget:
// its pinned worker, or every worker holding its skill and role, unpaused.
// why explains an empty result.
func (d *Dispatcher) candidates(st *tickState, it Item) (out []Worker, why string) {
	pool := st.workers
	if it.Worker != "" {
		w, ok := st.byID[it.Worker]
		if !ok {
			return nil, "worker " + it.Worker + " is not available"
		}
		pool = []Worker{w}
	}
	paused := 0
	for _, w := range pool {
		if it.RequiredSkill != "" && !contains(w.Skills, it.RequiredSkill) {
			continue
		}
		if it.RequiredRole != "" && !contains(w.Roles, it.RequiredRole) {
			continue
		}
		if capacityOf(st.caps, w.ID).Paused {
			paused++
			continue
		}
		out = append(out, w)
	}
	if len(out) > 0 {
		return out, ""
	}
	switch {
	case paused > 0:
		return nil, "every worker that can take it is paused"
	case it.Worker != "":
		return nil, "worker " + it.Worker + " lacks the required skill or role"
	default:
		var need []string
		if it.RequiredSkill != "" {
			need = append(need, "skill "+it.RequiredSkill)
		}
		if it.RequiredRole != "" {
			need = append(need, "role "+it.RequiredRole)
		}
		if len(need) == 0 {
			return nil, "no worker is available"
		}
		return nil, "no worker holds " + strings.Join(need, " and ")
	}
}

// contains reports whether list holds v, ignoring case.
func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

// LimitEnforcement says, for the API and the page, what each kind of limit is checked against (review RW1-F-02): the
// token limit against what the model proxy itself counted, the dollar limit only against the cost each desktop reports
// when a turn ends - no price is known to the control plane, so it cannot meter dollars.
const LimitEnforcement = "Token limits are enforced at the model proxy, which counts every hosted call as it is made. " +
	"Dollar limits are compared with the cost each desktop reports about itself when a task ends: not metered, so set a token limit to cap spend."

// OverBudget says why worker is at or over a spend limit for the current day or month, "" when it is under every
// limit that applies, read now - what the model proxy asks before it forwards a call (C-08).
func (d *Dispatcher) OverBudget(worker string) (string, error) {
	limits, err := d.Store.Limits()
	if err != nil {
		return "", err
	}
	return d.overBudget(&tickState{limits: limits}, worker)
}

// overBudget says why a worker is at or over a spend limit for the current
// day or month, "" when it is under every limit that applies. It is read
// afresh for every item, never from a reading taken earlier in the tick: with
// spend metered as it happens, a second item must see what the first one's
// worker has used by then (C-08).
func (d *Dispatcher) overBudget(st *tickState, worker string) (string, error) {
	why := ""
	now := d.now()
	for _, period := range []string{PeriodDay, PeriodMonth} {
		l, ok := limitFor(st.limits, worker, period)
		if !ok {
			continue
		}
		if d.Spend == nil {
			why = "spend limits are set but spend cannot be read"
			break
		}
		spent, err := d.Spend.Spend(worker, d.PeriodStart(period, now))
		if err != nil {
			return "", fmt.Errorf("reading spend of %s: %w", worker, err)
		}
		if l.MaxUSD > 0 && spent.USD >= l.MaxUSD {
			why = fmt.Sprintf("%s has spent $%.2f of its $%.2f %s limit (the cost its desktop reported)", worker, spent.USD, l.MaxUSD, period)
			break
		}
		if l.MaxTokens > 0 && spent.Tokens >= l.MaxTokens {
			why = fmt.Sprintf("%s has used %d of its %d-token %s limit", worker, spent.Tokens, l.MaxTokens, period)
			break
		}
	}
	return why, nil
}

// affordable splits candidates into those under budget, with the reasons
// the others are not.
func (d *Dispatcher) affordable(st *tickState, cands []Worker) ([]Worker, []string, error) {
	var ok []Worker
	var over []string
	for _, w := range cands {
		why, err := d.overBudget(st, w.ID)
		if err != nil {
			return nil, nil, err
		}
		if why != "" {
			over = append(over, why)
			continue
		}
		ok = append(ok, w)
	}
	return ok, over, nil
}

// releaseHeld returns budget-held items to the queue once some worker that
// could take them is under its limits again (a new day or month, a raised
// limit).
func (d *Dispatcher) releaseHeld(st *tickState) error {
	held, err := d.Store.inStatus(StatusHeld)
	if err != nil {
		return err
	}
	for _, it := range held {
		if it.Hold != HoldBudget {
			continue
		}
		cands, _ := d.candidates(st, it)
		ok, _, err := d.affordable(st, cands)
		if err != nil {
			return err
		}
		if len(ok) > 0 {
			if err := d.Store.release(it.ID, "back in the queue: a worker is under its spend limit again"); err != nil {
				return err
			}
		}
	}
	return nil
}

// isBusy says (once per Tick per worker) why a worker is busy, "" when free.
func (d *Dispatcher) isBusy(st *tickState, worker string) (string, error) {
	if why, ok := st.busy[worker]; ok {
		return why, nil
	}
	why, err := d.Workers.Busy(worker)
	if err != nil {
		return "", err
	}
	st.busy[worker] = why
	return why, nil
}

// assign walks the queue in priority order and sends each item it can.
func (d *Dispatcher) assign(ctx context.Context, st *tickState) error {
	queued, err := d.Store.inStatus(StatusQueued)
	if err != nil {
		return err
	}
	now := d.now()
	for _, it := range queued {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !it.NotBefore.IsZero() && now.Before(it.NotBefore) {
			continue
		}
		cands, why := d.candidates(st, it)
		if len(cands) == 0 {
			if err := d.Store.note(it.ID, StatusQueued, "waiting: "+why); err != nil {
				return err
			}
			continue
		}
		ok, over, err := d.affordable(st, cands)
		if err != nil {
			return err
		}
		if len(ok) == 0 {
			if err := d.Store.hold(it.ID, []string{StatusQueued}, HoldBudget, "over budget: "+strings.Join(over, "; ")); err != nil {
				return err
			}
			continue
		}
		w, why, err := d.pick(st, ok)
		if err != nil {
			return err
		}
		if w == "" {
			if err := d.Store.note(it.ID, StatusQueued, "waiting: "+why); err != nil {
				return err
			}
			continue
		}
		if err := d.send(ctx, st, it, w); err != nil {
			return err
		}
	}
	return nil
}

// pick chooses the least-loaded free worker among ok, or says why none is free.
func (d *Dispatcher) pick(st *tickState, ok []Worker) (string, string, error) {
	sort.SliceStable(ok, func(i, j int) bool { return st.inFlight[ok[i].ID] < st.inFlight[ok[j].ID] })
	reasons := []string{}
	for _, w := range ok {
		if st.inFlight[w.ID] >= capacityOf(st.caps, w.ID).MaxConcurrent {
			reasons = append(reasons, w.ID+" is at its capacity")
			continue
		}
		// Busy also sees the worker's own queue items, so it is asked only of a
		// worker with none in flight: above that, capacity decides and the
		// submission path refuses a send it cannot take (a busy refusal).
		if st.inFlight[w.ID] > 0 {
			return w.ID, "", nil
		}
		busy, err := d.isBusy(st, w.ID)
		if err != nil {
			log.Printf("workqueue: checking %s: %v", w.ID, err)
			reasons = append(reasons, w.ID+" could not be checked")
			continue
		}
		if busy != "" {
			reasons = append(reasons, w.ID+" is busy")
			continue
		}
		return w.ID, "", nil
	}
	return "", strings.Join(reasons, "; "), nil
}

// send claims an item for worker and submits it; a refused send puts it back.
func (d *Dispatcher) send(ctx context.Context, st *tickState, it Item, worker string) error {
	claimed, err := d.Store.claim(it.ID, worker)
	if err != nil || !claimed {
		return err
	}
	st.inFlight[worker]++

	files, err := d.Store.Files(it.ID)
	if err != nil {
		return err
	}
	request := it.Request
	dir := ""
	if len(files) > 0 {
		dir = InboxDir(it.ID)
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.Name
		}
		request += "\n\nFiles for this request are in " + dir + ": " + strings.Join(names, ", ")
	}

	taskID, err := d.Submitter.Submit(tasks.WithOrigin(ctx, tasks.Origin{Kind: tasks.OriginQueue, Ref: it.ID}), worker, request, dir, files)
	if err == nil && taskID == "" {
		err = errors.New("no task id came back")
	}
	if err == nil {
		_, err = d.Store.started(it.ID, taskID)
		return err
	}

	st.inFlight[worker]--
	var busy interface{ DesktopBusy() string }
	if errors.As(err, &busy) {
		st.busy[worker] = busy.DesktopBusy()
		// Nothing more goes to it this tick, whatever its capacity says.
		st.inFlight[worker] = capacityOf(st.caps, worker).MaxConcurrent
		return d.Store.unassign(it.ID, "waiting: "+worker+" is busy", time.Time{}, true)
	}
	log.Printf("workqueue: sending %s to %s: %v", it.ID, worker, err)
	if it.Attempts+1 >= it.MaxAttempts {
		return d.Store.finish(it.ID, StatusFailed, fmt.Sprintf("could not be sent after %d attempts", it.Attempts+1))
	}
	backoff := d.Backoff
	if backoff <= 0 {
		backoff = time.Minute
	}
	return d.Store.unassign(it.ID, "sending to "+worker+" failed; trying again", d.now().Add(backoff), false)
}
