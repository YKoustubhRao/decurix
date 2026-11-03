package mailtrigger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func TestPasswordsAreEncryptedAtRestAndAKeyLossAsksForThePassword(t *testing.T) {
	h := newHarness(t)

	var stored string
	if err := h.db.QueryRow(`SELECT password FROM mail_accounts WHERE id = ?`, h.acct.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(stored, sealedPrefix) || strings.Contains(stored, "abcdefghijklmnop") {
		t.Fatalf("stored password = %q, want it sealed", stored)
	}

	acct, err := h.store.Account(h.acct.ID)
	if err != nil || acct.password != "abcdefghijklmnop" {
		t.Fatalf("round trip = %q, %v", acct.password, err)
	}

	// The same database opened with another key (the key file was lost).
	other, err := NewStore(h.db, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	acct, err = other.Account(h.acct.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !acct.NeedsPassword || acct.Status != "needs_password" || !strings.Contains(acct.LastError, "enter it again") {
		t.Errorf("account with an unreadable password = %+v", acct)
	}
}

func TestAPasswordSavedBeforeThereWasAKeyIsEncryptedOnStart(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	plain, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}

	acct, err := plain.CreateAccount(NewAccount{Email: "a@b.c", Password: "hunter2", Host: "h", Port: 993, Folder: "INBOX"}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}

	key, _ := LoadKey(t.TempDir())
	sealed, err := NewStore(db, key)
	if err != nil {
		t.Fatal(err)
	}

	var stored string
	_ = db.QueryRow(`SELECT password FROM mail_accounts`).Scan(&stored)
	if strings.Contains(stored, "hunter2") {
		t.Fatalf("still plain: %q", stored)
	}

	if got, _ := sealed.Account(acct.ID); got.password != "hunter2" {
		t.Errorf("password = %q", got.password)
	}
}

func TestLoadKeyIsStable(t *testing.T) {
	dir := t.TempDir()

	a, err := LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	b, err := LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	if string(a) != string(b) || len(a) != 32 {
		t.Fatal("the key changed between loads")
	}
}

func TestARefusedPasswordStopsPollingUntilItIsReplaced(t *testing.T) {
	h := newHarness(t)

	h.box.mu.Lock()
	h.box.dialErr = fmt.Errorf("%w (Invalid credentials)", ErrAuth)
	h.box.mu.Unlock()

	h.advance(2 * time.Minute)
	h.runner.step(context.Background())

	acct, _ := h.store.Account(h.acct.ID)
	if acct.Status != "needs_password" {
		t.Fatalf("status = %q", acct.Status)
	}

	if h.store.Active() {
		t.Error("a mailbox that is not polled still keeps the machine awake")
	}

	dials := h.box.dials
	for i := 0; i < 5; i++ {
		h.advance(10 * time.Minute)
		h.runner.step(context.Background())
	}

	if h.box.dials != dials {
		t.Fatalf("logged in %d more times with a refused password", h.box.dials-dials)
	}

	h.box.mu.Lock()
	h.box.dialErr = nil
	h.box.mu.Unlock()

	if err := h.store.UpdatePassword(h.acct.ID, "new"); err != nil {
		t.Fatal(err)
	}

	h.advance(2 * time.Minute)
	h.runner.step(context.Background())

	if acct, _ := h.store.Account(h.acct.ID); acct.Status != "connected" {
		t.Fatalf("after a new password: %q (%s)", acct.Status, acct.LastError)
	}
}

func TestAnUnreachableServerIsPolledLessAndLessOften(t *testing.T) {
	h := newHarness(t)

	h.box.mu.Lock()
	h.box.dialErr = errors.New("connection refused")
	h.box.mu.Unlock()

	// Every minute for an hour: without backoff that is 60 attempts.
	for i := 0; i < 60; i++ {
		h.advance(time.Minute)
		h.runner.lastRun = *h.clock // no sleep gap
		h.runner.step(context.Background())
	}

	if h.box.dials > 12 {
		t.Fatalf("%d attempts in an hour against a server that is down", h.box.dials)
	}

	acct, _ := h.store.Account(h.acct.ID)
	if acct.Status != "error" || acct.Failures < 4 {
		t.Errorf("account = %+v", acct)
	}
}

func TestAFloodIsWorkedThroughInOnePoll(t *testing.T) {
	h := newHarness(t)
	h.rule(t, NewRule{SubjectRe: `^order`})

	for i := 0; i < 180; i++ {
		h.box.deliver("shop@x.com", fmt.Sprintf("Order %d", i), "")
	}

	h.advance(2 * time.Minute)
	h.runner.step(context.Background())

	jobs, _ := h.store.Jobs(500)
	if len(jobs) != 180 {
		t.Fatalf("jobs = %d, want 180", len(jobs))
	}
}

func TestTwoDispatchersNeverSendTheSameEmailTwice(t *testing.T) {
	h := newFileHarness(t)
	h.rule(t, NewRule{SubjectRe: `.`})
	h.sub.stickBusy = false

	h.box.deliver("a@x.com", "Once", "")
	h.advance(2 * time.Minute)
	if err := h.runner.poll(context.Background(), h.acct); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; h.runner.dispatch(context.Background()) }()
	}
	close(start)
	wg.Wait()

	if n := h.sub.count(); n != 1 {
		t.Fatalf("sent %d times", n)
	}
}

func TestAFailedSendIsRetriedWithBackoffThenFailed(t *testing.T) {
	h := newHarness(t)
	h.rule(t, NewRule{SubjectRe: `.`})
	h.sub.err = errors.New("the desktop did not answer")

	h.box.deliver("a@x.com", "Retry me", "")
	h.advance(2 * time.Minute)
	h.runner.step(context.Background())

	job := h.jobs(t)[0]
	if job.Status != JobPending || job.Attempts != 1 || !strings.Contains(job.Note, "attempt 1 of 4") {
		t.Fatalf("after one failure: %+v", job)
	}

	// Not before its time.
	h.advance(30 * time.Second)
	h.runner.dispatch(context.Background())
	if job := h.jobs(t)[0]; job.Attempts != 1 {
		t.Fatalf("retried early: %+v", job)
	}

	for _, wait := range sendBackoff {
		h.advance(wait)
		h.runner.dispatch(context.Background())
	}

	job = h.jobs(t)[0]
	if job.Status != JobFailed || job.Attempts != 4 {
		t.Fatalf("after every retry: %+v", job)
	}
}

func TestAJobInterruptedMidSendIsNeverResentOnItsOwn(t *testing.T) {
	h := newHarness(t)
	h.rule(t, NewRule{SubjectRe: `.`})

	h.box.deliver("a@x.com", "Pay invoice", "")
	h.advance(2 * time.Minute)
	_ = h.runner.poll(context.Background(), h.acct)

	job := h.jobs(t)[0]
	if ok, _ := h.store.claimJob(job.ID, *h.clock); !ok {
		t.Fatal("could not claim")
	}

	// The control plane stops here, and starts again.
	n, err := h.store.RecoverInterrupted()
	if err != nil || n != 1 {
		t.Fatalf("recovered %d, %v", n, err)
	}

	h.runner.dispatch(context.Background())

	if h.sub.count() != 0 {
		t.Fatal("an interrupted send was sent again without a person deciding to")
	}

	if job := h.jobs(t)[0]; job.Status != JobFailed || !strings.Contains(job.Note, "Check the desktop") {
		t.Fatalf("job = %+v", job)
	}
}

func TestARuleDeletedDuringAPollQueuesNothing(t *testing.T) {
	h := newHarness(t)
	rule := h.rule(t, NewRule{SubjectRe: `.`})

	h.box.deliver("a@x.com", "Hi", "")
	h.advance(2 * time.Minute)

	acct, _ := h.store.Account(h.acct.ID)
	p := h.runner.read(context.Background(), acct)
	if len(p.Jobs) != 1 {
		t.Fatalf("read %d jobs", len(p.Jobs))
	}

	if err := h.store.DeleteRule(rule.ID); err != nil {
		t.Fatal(err)
	}

	if err := h.store.RecordPoll(h.acct.ID, p); err != nil {
		t.Fatal(err)
	}

	if jobs := h.jobs(t); len(jobs) != 0 {
		t.Fatalf("queued %d jobs for a deleted rule", len(jobs))
	}
}
