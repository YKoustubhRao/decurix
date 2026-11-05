package workqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeWorkers struct {
	list []Worker
	busy map[string]string
}

func (f *fakeWorkers) Workers(context.Context) ([]Worker, error) {
	return append([]Worker(nil), f.list...), nil
}
func (f *fakeWorkers) Busy(w string) (string, error) { return f.busy[w], nil }

type busyErr struct{}

func (busyErr) Error() string       { return "busy" }
func (busyErr) DesktopBusy() string { return "a task is running" }

type sent struct {
	worker, request, dir string
	files                []File
	taskID               string
}

type fakeSubmitter struct {
	calls []sent
	fail  map[string]error
	n     int
	busy  *fakeWorkers // marks a worker busy once sent to, like the real desktop
}

func (f *fakeSubmitter) Submit(_ context.Context, worker, request, dir string, files []File, _ tasks.Origin) (string, error) {
	if err := f.fail[worker]; err != nil {
		return "", err
	}
	f.n++
	id := fmt.Sprintf("t%d", f.n)
	f.calls = append(f.calls, sent{worker, request, dir, files, id})
	if f.busy != nil {
		f.busy.busy[worker] = "running " + id
	}
	return id, nil
}

type fakeTasks struct{ m map[string]TaskState }

func (f *fakeTasks) TaskState(id string) (TaskState, error) {
	s, ok := f.m[id]
	if !ok {
		return TaskState{Found: true, Status: "running"}, nil
	}
	return s, nil
}

type spendEntry struct {
	worker string
	at     time.Time
	s      Spend
}

type fakeSpend struct{ entries []spendEntry }

func (f *fakeSpend) Spend(worker string, since time.Time) (Spend, error) {
	var out Spend
	for _, e := range f.entries {
		if e.worker == worker && !e.at.Before(since) {
			out.USD += e.s.USD
			out.Tokens += e.s.Tokens
		}
	}
	return out, nil
}

type env struct {
	store   *Store
	clock   *fakeClock
	workers *fakeWorkers
	sub     *fakeSubmitter
	tasks   *fakeTasks
	spend   *fakeSpend
	d       *Dispatcher
}

func newEnv(t *testing.T, workers ...Worker) *env {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		store:   store,
		clock:   &fakeClock{t: time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)},
		workers: &fakeWorkers{list: workers, busy: map[string]string{}},
		tasks:   &fakeTasks{m: map[string]TaskState{}},
		spend:   &fakeSpend{},
	}
	e.sub = &fakeSubmitter{fail: map[string]error{}, busy: e.workers}
	store.SetClock(e.clock.now)
	e.d = &Dispatcher{Store: store, Workers: e.workers, Submitter: e.sub, Tasks: e.tasks, Spend: e.spend,
		Clock: e.clock.now, Location: time.UTC}
	return e
}

func (e *env) tick(t *testing.T) {
	t.Helper()
	if err := e.d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *env) add(t *testing.T, in NewItem) Item {
	t.Helper()
	if in.Request == "" {
		in.Request = "do the thing"
	}
	it, err := e.store.Enqueue(in)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.add(time.Second) // distinct created times
	return it
}

func (e *env) get(t *testing.T, id string) Item {
	t.Helper()
	it, err := e.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) finishTask(taskID string, s TaskState) {
	s.Found = true
	e.tasks.m[taskID] = s
	for w, why := range e.workers.busy {
		if why == "running "+taskID {
			delete(e.workers.busy, w)
		}
	}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func TestPriorityOrder(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	low := e.add(t, NewItem{Request: "low", Priority: 1})
	high := e.add(t, NewItem{Request: "high", Priority: 9})
	mid := e.add(t, NewItem{Request: "mid", Priority: 5})

	e.tick(t)
	if len(e.sub.calls) != 1 || e.sub.calls[0].request != "high" {
		t.Fatalf("first send = %+v, want high", e.sub.calls)
	}
	if got := e.get(t, high.ID); got.Status != StatusRunning || got.TaskID != "t1" || got.AssignedWorker != "w1" || got.Attempts != 1 {
		t.Fatalf("high = %+v", got)
	}
	if got := e.get(t, low.ID); got.Status != StatusQueued || got.Reason == "" {
		t.Fatalf("low should wait with a reason: %+v", got)
	}

	e.finishTask("t1", TaskState{Status: "done", Checked: yes()})
	e.tick(t)
	if got := e.get(t, high.ID); got.Status != StatusDone {
		t.Fatalf("high = %s, want done", got.Status)
	}
	if e.sub.calls[1].request != "mid" {
		t.Fatalf("second send = %q, want mid", e.sub.calls[1].request)
	}
	_ = mid
}

func TestFIFOWithinPriorityAndNotBefore(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	later := e.add(t, NewItem{Request: "later", NotBefore: e.clock.now().Add(time.Hour)})
	first := e.add(t, NewItem{Request: "first"})
	e.add(t, NewItem{Request: "second"})
	e.tick(t)
	if e.sub.calls[0].request != "first" {
		t.Fatalf("sent %q, want first", e.sub.calls[0].request)
	}
	if got := e.get(t, later.ID); got.Status != StatusQueued {
		t.Fatalf("not_before item moved: %s", got.Status)
	}
	_ = first
}

func TestCapacityPausedAndBusy(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"}, Worker{ID: "w2"}, Worker{ID: "w3"})
	if _, err := e.store.SetCapacity(Capacity{Worker: "w2", Paused: true}); err != nil {
		t.Fatal(err)
	}
	e.workers.busy["w3"] = "a person's task"
	a := e.add(t, NewItem{})
	b := e.add(t, NewItem{})
	e.tick(t)
	if len(e.sub.calls) != 1 || e.sub.calls[0].worker != "w1" {
		t.Fatalf("calls = %+v, want one to w1", e.sub.calls)
	}
	if got := e.get(t, b.ID); got.Status != StatusQueued {
		t.Fatalf("b = %s", got.Status)
	}
	// A second tick does not send more to w1: it is at capacity (and busy).
	e.tick(t)
	if len(e.sub.calls) != 1 {
		t.Fatalf("sent past capacity: %+v", e.sub.calls)
	}
	// Unpausing w2 frees a slot.
	if _, err := e.store.SetCapacity(Capacity{Worker: "w2", MaxConcurrent: 1}); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if len(e.sub.calls) != 2 || e.sub.calls[1].worker != "w2" {
		t.Fatalf("calls = %+v, want second to w2", e.sub.calls)
	}
	_ = a
}

func TestCapacityAboveOne(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	e.sub.busy = nil // a worker that can run several at once
	if _, err := e.store.SetCapacity(Capacity{Worker: "w1", MaxConcurrent: 2}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		e.add(t, NewItem{})
	}
	e.tick(t)
	if len(e.sub.calls) != 2 {
		t.Fatalf("sent %d, want 2", len(e.sub.calls))
	}
}

func TestPinnedWorker(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"}, Worker{ID: "w2"})
	pinned := e.add(t, NewItem{Worker: "w2"})
	gone := e.add(t, NewItem{Worker: "w9"})
	e.tick(t)
	if len(e.sub.calls) != 1 || e.sub.calls[0].worker != "w2" {
		t.Fatalf("calls = %+v", e.sub.calls)
	}
	if got := e.get(t, gone.ID); got.Status != StatusQueued || got.Reason != "waiting: worker w9 is not available" {
		t.Fatalf("gone = %+v", got)
	}
	// A second item pinned to busy w2 waits even though w1 is free.
	other := e.add(t, NewItem{Worker: "w2"})
	e.tick(t)
	if got := e.get(t, other.ID); got.Status != StatusQueued {
		t.Fatalf("other = %s", got.Status)
	}
	_ = pinned
}

func TestSkillAndRoleEligibility(t *testing.T) {
	e := newEnv(t,
		Worker{ID: "w1", Roles: []string{"Accounts"}},
		Worker{ID: "w2", Skills: []string{"monthly-report"}},
		Worker{ID: "w3", Roles: []string{"accounts"}, Skills: []string{"monthly-report"}})
	both := e.add(t, NewItem{RequiredSkill: "monthly-report", RequiredRole: "accounts", Priority: 3})
	skill := e.add(t, NewItem{RequiredSkill: "monthly-report", Priority: 2})
	role := e.add(t, NewItem{RequiredRole: "accounts", Priority: 1})
	none := e.add(t, NewItem{RequiredSkill: "nobody-has"})
	e.tick(t)

	want := map[string]string{both.ID: "w3", skill.ID: "w2", role.ID: "w1"}
	for id, w := range want {
		if got := e.get(t, id); got.AssignedWorker != w || got.Status != StatusRunning {
			t.Fatalf("%s went to %q (%s), want %s", id, got.AssignedWorker, got.Status, w)
		}
	}
	if got := e.get(t, none.ID); got.Status != StatusQueued || got.Reason != "waiting: no worker holds skill nobody-has" {
		t.Fatalf("none = %+v", got)
	}
}

func TestSpendLimitDayHoldsAndReleases(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	if _, err := e.store.SetLimit(Limit{Worker: "w1", Period: PeriodDay, MaxUSD: 5}); err != nil {
		t.Fatal(err)
	}
	// Spent $6 earlier today, $100 yesterday (outside the day).
	e.spend.entries = []spendEntry{
		{"w1", e.clock.now().Add(-time.Hour), Spend{USD: 6}},
		{"w1", e.clock.now().Add(-24 * time.Hour), Spend{USD: 100}},
	}
	it := e.add(t, NewItem{})
	e.tick(t)
	got := e.get(t, it.ID)
	if got.Status != StatusHeld || got.Hold != HoldBudget || got.Reason == "" {
		t.Fatalf("item = %+v, want held over budget", got)
	}
	if len(e.sub.calls) != 0 {
		t.Fatal("sent while over budget")
	}
	// Still held later the same day.
	e.clock.add(time.Hour)
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusHeld {
		t.Fatalf("released early: %s", got.Status)
	}
	// Next day: released and sent.
	e.clock.add(12 * time.Hour)
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusRunning {
		t.Fatalf("after midnight = %+v, want running", got)
	}
}

func TestSpendLimitMonthTokensAndDefaultRow(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"}, Worker{ID: "w2"})
	if _, err := e.store.SetLimit(Limit{Worker: AnyWorker, Period: PeriodMonth, MaxTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	// w2 has its own, higher month limit; the "*" row does not apply to it.
	if _, err := e.store.SetLimit(Limit{Worker: "w2", Period: PeriodMonth, MaxTokens: 5000}); err != nil {
		t.Fatal(err)
	}
	early := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) // this month, not today
	e.spend.entries = []spendEntry{
		{"w1", early, Spend{Tokens: 1200}},
		{"w2", early, Spend{Tokens: 1200}},
		{"w1", time.Date(2026, 2, 27, 0, 0, 0, 0, time.UTC), Spend{Tokens: 99999}},
	}
	pinned := e.add(t, NewItem{Worker: "w1"})
	free := e.add(t, NewItem{})
	e.tick(t)
	if got := e.get(t, pinned.ID); got.Status != StatusHeld {
		t.Fatalf("pinned to over-budget w1 = %+v, want held", got)
	}
	if got := e.get(t, free.ID); got.Status != StatusRunning || got.AssignedWorker != "w2" {
		t.Fatalf("unpinned = %+v, want running on w2", got)
	}
	// Raising the "*" limit releases the held item.
	if _, err := e.store.SetLimit(Limit{Worker: AnyWorker, Period: PeriodMonth, MaxTokens: 2000}); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if got := e.get(t, pinned.ID); got.Status != StatusRunning {
		t.Fatalf("after raise = %+v", got)
	}
}

func TestBusyOverBudgetIsNotHeldWhileAnotherWorkerIsUnder(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"}, Worker{ID: "w2"})
	if _, err := e.store.SetLimit(Limit{Worker: "w1", Period: PeriodDay, MaxUSD: 1}); err != nil {
		t.Fatal(err)
	}
	e.spend.entries = []spendEntry{{"w1", e.clock.now(), Spend{USD: 2}}}
	e.workers.busy["w2"] = "busy"
	it := e.add(t, NewItem{})
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusQueued {
		t.Fatalf("item = %+v, want queued waiting for w2", got)
	}
}

func TestDoneOnlyWhenChecked(t *testing.T) {
	cases := []struct {
		name  string
		state TaskState
		want  string
	}{
		{"checked", TaskState{Status: "done", Checked: yes()}, StatusDone},
		{"unchecked", TaskState{Status: "done", Checked: no()}, StatusNotDone},
		{"not said", TaskState{Status: "done"}, StatusNotDone},
		{"person correct", TaskState{Status: "done", Verdict: "correct"}, StatusDone},
		{"checked but wrong", TaskState{Status: "done", Checked: yes(), Verdict: "wrong"}, StatusNotDone},
		{"error", TaskState{Status: "error", Reply: "could not log in\nmore"}, StatusFailed},
		{"query", TaskState{Status: "query"}, StatusNotDone},
		{"abandoned", TaskState{Status: "abandoned"}, StatusNotDone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Worker{ID: "w1"})
			it := e.add(t, NewItem{})
			e.tick(t)
			e.finishTask("t1", c.state)
			e.tick(t)
			if got := e.get(t, it.ID); got.Status != c.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Reason, c.want)
			}
		})
	}
}

func TestWaitingStaysRunning(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	it := e.add(t, NewItem{})
	e.tick(t)
	e.tasks.m["t1"] = TaskState{Found: true, Status: "waiting"}
	e.tick(t)
	got := e.get(t, it.ID)
	if got.Status != StatusRunning || got.Reason != "waiting on a person in the worker's chat" {
		t.Fatalf("got %+v", got)
	}
	e.tasks.m["t1"] = TaskState{}
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusFailed {
		t.Fatalf("missing task record = %s", got.Status)
	}
}

func TestBusyRefusalRequeuesWithoutCountingAttempt(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	e.sub.fail["w1"] = busyErr{}
	it := e.add(t, NewItem{})
	e.tick(t)
	got := e.get(t, it.ID)
	if got.Status != StatusQueued || got.Attempts != 0 || got.AssignedWorker != "" {
		t.Fatalf("got %+v", got)
	}
	delete(e.sub.fail, "w1")
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusRunning || got.Attempts != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestSendFailureBacksOffThenFails(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	e.sub.fail["w1"] = errors.New("docker gone")
	it := e.add(t, NewItem{MaxAttempts: 2})
	e.tick(t)
	got := e.get(t, it.ID)
	if got.Status != StatusQueued || got.Attempts != 1 || !got.NotBefore.After(e.clock.now()) {
		t.Fatalf("after first failure %+v", got)
	}
	e.tick(t) // inside backoff: nothing
	if got := e.get(t, it.ID); got.Attempts != 1 {
		t.Fatalf("retried inside backoff: %+v", got)
	}
	e.clock.add(2 * time.Minute)
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusFailed || got.Attempts != 2 {
		t.Fatalf("after second failure %+v", got)
	}
}

func TestRetryAndCancel(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	it := e.add(t, NewItem{})
	e.tick(t)
	e.finishTask("t1", TaskState{Status: "error"})
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusFailed {
		t.Fatalf("got %s", got.Status)
	}
	r, err := e.store.Retry(it.ID)
	if err != nil || r.Status != StatusQueued || r.TaskID != "" || r.AssignedWorker != "" {
		t.Fatalf("retry = %+v, %v", r, err)
	}
	e.tick(t)
	got := e.get(t, it.ID)
	if got.Status != StatusRunning || got.TaskID != "t2" || got.Attempts != 2 {
		t.Fatalf("after retry %+v", got)
	}
	e.finishTask("t2", TaskState{Status: "done", Checked: yes()})
	e.tick(t)
	if _, err := e.store.Retry(it.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of done = %v, want conflict", err)
	}
	if _, err := e.store.Cancel(it.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel of done = %v, want conflict", err)
	}

	q := e.add(t, NewItem{Worker: "w9"})
	c, err := e.store.Cancel(q.ID, "not needed")
	if err != nil || c.Status != StatusCancelled || c.Reason != "not needed" {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	e.tick(t)
	if got := e.get(t, q.ID); got.Status != StatusCancelled {
		t.Fatalf("cancelled item moved: %s", got.Status)
	}
	if _, err := e.store.Cancel("qnope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown = %v", err)
	}
}

func TestCancelRunningStopsTracking(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	it := e.add(t, NewItem{})
	e.tick(t)
	if _, err := e.store.Cancel(it.ID, ""); err != nil {
		t.Fatal(err)
	}
	e.finishTask("t1", TaskState{Status: "done", Checked: yes()})
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusCancelled {
		t.Fatalf("got %s", got.Status)
	}
}

func TestInterruptedSendIsHeldNotResent(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	it := e.add(t, NewItem{})
	if ok, err := e.store.claim(it.ID, "w1"); !ok || err != nil {
		t.Fatalf("claim %v %v", ok, err)
	}
	e.tick(t)
	got := e.get(t, it.ID)
	if got.Status != StatusHeld || got.Hold != HoldInterrupted {
		t.Fatalf("got %+v", got)
	}
	if len(e.sub.calls) != 0 {
		t.Fatal("resent an item that may already have reached the desktop")
	}
	if _, err := e.store.Retry(it.ID); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if got := e.get(t, it.ID); got.Status != StatusRunning {
		t.Fatalf("got %s", got.Status)
	}
}

func TestFilesGoToInbox(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	it := e.add(t, NewItem{Request: "read these", Files: []File{{Name: "b.csv", Data: []byte("1,2")}, {Name: "a.txt", Data: []byte("x")}}})
	if len(it.Files) != 2 || it.Files[0].Name != "a.txt" || it.Files[1].Size != 3 {
		t.Fatalf("files = %+v", it.Files)
	}
	e.tick(t)
	c := e.sub.calls[0]
	if c.dir != InboxDir(it.ID) || len(c.files) != 2 || string(c.files[1].Data) != "1,2" {
		t.Fatalf("sent %+v", c)
	}
	want := "read these\n\nFiles for this request are in " + InboxDir(it.ID) + ": a.txt, b.csv"
	if c.request != want {
		t.Fatalf("request = %q", c.request)
	}
}

func TestEnqueueValidation(t *testing.T) {
	e := newEnv(t)
	bad := []NewItem{
		{Request: "  "},
		{Request: "x", Worker: "../etc"},
		{Request: "x", RequiredSkill: "bad/skill"},
		{Request: "x", Files: []File{{Name: "../up", Data: nil}}},
		{Request: "x", Files: []File{{Name: "a"}, {Name: "a"}}},
		{Request: "x", MaxAttempts: 99},
	}
	for _, in := range bad {
		if _, err := e.store.Enqueue(in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Enqueue(%+v) = %v, want invalid", in, err)
		}
	}
	if _, err := e.store.SetLimit(Limit{Worker: "w1", Period: "week", MaxUSD: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatal("week accepted")
	}
	if _, err := e.store.SetCapacity(Capacity{Worker: "w1", MaxConcurrent: 99}); !errors.Is(err, ErrInvalid) {
		t.Fatal("huge capacity accepted")
	}
}

func TestPeriodStartUsesLocation(t *testing.T) {
	loc := time.FixedZone("plus5", 5*3600)
	d := &Dispatcher{Location: loc}
	at := time.Date(2026, 3, 31, 20, 0, 0, 0, time.UTC) // already April 1st at +5
	if got := d.PeriodStart(PeriodDay, at); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("day start %v", got)
	}
	if got := d.PeriodStart(PeriodMonth, at); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("month start %v", got)
	}
}

func TestTaskSpendReadsTaskRecords(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts, err := tasks.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	w := tasks.Worker{ID: "w1", Name: "w1"}
	for _, cost := range []float64{0.5, 1.25} {
		route, err := ts.Commit(tasks.NewRoute(), w, "x", tasks.Origin{Kind: tasks.OriginQueue})
		if err != nil {
			t.Fatal(err)
		}
		c, in, out := cost, int64(100), int64(20)
		if _, err := ts.RecordOutcome("w1", route.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, CostUSD: &c, InputTokens: &in, OutputTokens: &out}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := TaskSpend{DB: db}.Spend("w1", time.Now().Add(-time.Hour))
	if err != nil || got.USD != 1.75 || got.Tokens != 240 {
		t.Fatalf("spend = %+v, %v", got, err)
	}
	if got, _ := (TaskSpend{DB: db}).Spend("w1", time.Now().Add(time.Hour)); got.USD != 0 {
		t.Fatalf("future window = %+v", got)
	}
	if got, _ := (TaskSpend{DB: db}).Spend("w2", time.Time{}); got.Tokens != 0 {
		t.Fatalf("other worker = %+v", got)
	}
}

func TestRunTicksAndStops(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	e.add(t, NewItem{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.d.Run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, _ := e.store.List(Filter{Status: StatusRunning})
		if len(items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not tick")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestCapacityAboveOneMeetsBusyRefusal(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	a := e.add(t, NewItem{})
	b := e.add(t, NewItem{})
	c := e.add(t, NewItem{})
	e.tick(t) // a is sent at capacity 1
	if _, err := e.store.SetCapacity(Capacity{Worker: "w1", MaxConcurrent: 3}); err != nil {
		t.Fatal(err)
	}
	// The submission path still takes one task at a time: b is refused busy, c is not tried this tick.
	e.sub.fail["w1"] = busyErr{}
	e.tick(t)
	if got := e.get(t, a.ID); got.Status != StatusRunning {
		t.Fatalf("a = %s", got.Status)
	}
	for _, id := range []string{b.ID, c.ID} {
		if got := e.get(t, id); got.Status != StatusQueued || got.Attempts != 0 {
			t.Fatalf("%s = %+v", id, got)
		}
	}
}
