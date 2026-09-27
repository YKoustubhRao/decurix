package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// fake is a control plane with a few desktops: a send makes the desktop
// busy with a task, and reply() is the desktop's agent answering.
type fake struct {
	desktops   []desktop.Desktop
	busy       map[string]string // desktop -> reason
	threads    map[string]*conversation.Thread
	taskOf     map[string]string // desktop -> current task id
	waiting    map[string]bool   // desktop -> waiting on a person
	sent       []string          // "desktop: text"
	taskStatus map[string]string
	suggest    string
	stopped    []string
	next       int
}

func newFake(desktops ...desktop.Desktop) *fake {
	return &fake{
		desktops:   desktops,
		busy:       map[string]string{},
		threads:    map[string]*conversation.Thread{},
		taskOf:     map[string]string{},
		waiting:    map[string]bool{},
		taskStatus: map[string]string{},
	}
}

func (f *fake) deps() Deps {
	return Deps{
		Desktops: func(context.Context) ([]desktop.Desktop, error) { return f.desktops, nil },
		Busy:     func(id string) (string, error) { return f.busy[id], nil },
		Send: func(_ context.Context, id, text string) (string, string, error) {
			f.sent = append(f.sent, id+": "+text)
			// Mirrors tasks.Plan: a desktop waiting on a person takes any
			// request as the reply to its open task.
			if f.waiting[id] {
				f.waiting[id] = false
				f.busy[id] = "running"
				return f.taskOf[id], "reply", nil
			}
			f.next++
			task := "t" + string(rune('0'+f.next))
			f.taskOf[id] = task
			f.busy[id] = "running"
			th := f.thread(id)
			th.Entries = append(th.Entries, conversation.Entry{ID: "u" + task, Role: conversation.RoleUser, TaskID: task, Content: text})
			th.Running = true
			return task, "new", nil
		},
		Transcript: func(id string) conversation.Thread { return *f.thread(id) },
		TaskStatus: func(task string) (string, error) { return f.taskStatus[task], nil },
		Suggest: func(context.Context, string) (string, bool) {
			return f.suggest, f.suggest != ""
		},
		Stop: func(_ context.Context, id string) error {
			f.stopped = append(f.stopped, id)
			return nil
		},
	}
}

func (f *fake) thread(id string) *conversation.Thread {
	if f.threads[id] == nil {
		f.threads[id] = &conversation.Thread{DesktopID: id}
	}
	return f.threads[id]
}

// reply is desktop id's agent answering its current task with kind.
func (f *fake) reply(id, kind, content string) {
	th := f.thread(id)
	th.Entries = append(th.Entries, conversation.Entry{
		ID: "a" + string(rune('0'+len(th.Entries))), Role: conversation.RoleAgent,
		Kind: kind, TaskID: f.taskOf[id], Content: content,
	})
	th.Running = false
	if kind == conversation.KindQuery || kind == conversation.KindApproval {
		f.waiting[id] = true
		f.busy[id] = "waiting on a person"
	} else {
		f.busy[id] = ""
	}
}

func newConcierge(t *testing.T, f *fake) *Concierge {
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
	return New(store, f.deps())
}

func running(id string) desktop.Desktop {
	return desktop.Desktop{ID: id, Name: id, Status: "running"}
}

func lastSaid(t *testing.T, c *Concierge) Message {
	t.Helper()
	msgs, err := c.Store.Messages(0, 1000)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("no messages (%v)", err)
	}
	return msgs[len(msgs)-1]
}

func send(t *testing.T, c *Concierge, text string) Message {
	t.Helper()
	if _, err := c.Handle(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	return lastSaid(t, c)
}

func TestNewTaskGoesToAFreeDesktopAndIsRelayedBack(t *testing.T) {
	f := newFake(running("alpha"), running("beta"))
	f.busy["alpha"] = "running"
	c := newConcierge(t, f)

	got := send(t, c, "reconcile the bank statement")
	if !strings.Contains(got.Text, "#1 started on beta") {
		t.Fatalf("got %q, want #1 started on beta", got.Text)
	}
	if f.sent[0] != "beta: reconcile the bank statement" {
		t.Fatalf("sent %v", f.sent)
	}

	f.reply("beta", conversation.KindEntry, "Reconciled: 42 entries, 0 mismatches.")
	c.Tick(context.Background())

	got = lastSaid(t, c)
	if got.Role != RoleAgent || got.SessionID != 1 || !strings.Contains(got.Text, "42 entries") {
		t.Fatalf("relay = %+v", got)
	}
	s, _ := c.Store.Session(1)
	if s.Status != StatusDone {
		t.Fatalf("status = %s, want done", s.Status)
	}

	// A second tick relays nothing new.
	c.Tick(context.Background())
	if again := lastSaid(t, c); again.ID != got.ID {
		t.Fatalf("relayed twice: %+v", again)
	}
}

func TestSuggestedDesktopIsPreferredOnlyWhenFree(t *testing.T) {
	f := newFake(running("alpha"), running("beta"))
	f.suggest = "beta"
	c := newConcierge(t, f)

	send(t, c, "first")
	if f.sent[0] != "beta: first" {
		t.Fatalf("sent %v, want beta first", f.sent)
	}
	send(t, c, "second") // beta is busy now
	if f.sent[1] != "alpha: second" {
		t.Fatalf("sent %v, want alpha second", f.sent)
	}
}

func TestNoFreeDesktopStartsNothing(t *testing.T) {
	f := newFake(running("alpha"), desktop.Desktop{ID: "dead", Name: "dead", Status: "stopped", Crash: "exit 1"})
	f.busy["alpha"] = "running"
	c := newConcierge(t, f)

	got := send(t, c, "do something")
	if !strings.Contains(got.Text, "Every desktop is busy") || len(f.sent) != 0 {
		t.Fatalf("got %q, sent %v", got.Text, f.sent)
	}
}

func TestAStoppedDesktopIsWokenForAJob(t *testing.T) {
	f := newFake(desktop.Desktop{ID: "sleepy", Name: "sleepy", Status: "stopped"})
	c := newConcierge(t, f)

	send(t, c, "check the price of gold")
	msgs, _ := c.Store.Messages(0, 100)
	if !strings.Contains(msgs[1].Text, "Waking sleepy up") {
		t.Fatalf("messages %+v", msgs)
	}
	if len(f.sent) != 1 {
		t.Fatalf("sent %v", f.sent)
	}
}

func TestAQuestionIsAnsweredByNumberAndByAShortYes(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "pay the invoice")
	f.reply("alpha", conversation.KindApproval, "NEEDS_APPROVAL: pay $400 to Acme?")
	c.Tick(context.Background())

	s, _ := c.Store.Session(1)
	if s.Status != StatusNeedsYou {
		t.Fatalf("status %s, want needs_you", s.Status)
	}

	got := send(t, c, "yes")
	if f.sent[1] != "alpha: yes" || !strings.Contains(got.Text, "Sent your reply to #1") {
		t.Fatalf("sent %v, said %q", f.sent, got.Text)
	}
	if s, _ = c.Store.Session(1); s.Status != StatusRunning {
		t.Fatalf("status %s, want running", s.Status)
	}

	f.reply("alpha", conversation.KindEntry, "Paid.")
	c.Tick(context.Background())
	if s, _ = c.Store.Session(1); s.Status != StatusDone {
		t.Fatalf("status %s, want done", s.Status)
	}

	// The same flow by number.
	send(t, c, "email the landlord")
	f.reply("alpha", conversation.KindQuery, "Which landlord?")
	c.Tick(context.Background())
	send(t, c, "#2 the one on Main St")
	if f.sent[len(f.sent)-1] != "alpha: the one on Main St" {
		t.Fatalf("sent %v", f.sent)
	}
}

func TestAShortYesIsNeverStartedAsANewJob(t *testing.T) {
	f := newFake(running("alpha"), running("beta"))
	c := newConcierge(t, f)

	if got := send(t, c, "yes"); !strings.Contains(got.Text, "Nothing is waiting") || len(f.sent) != 0 {
		t.Fatalf("said %q, sent %v", got.Text, f.sent)
	}

	// Two jobs waiting: ask which, send nothing.
	send(t, c, "@alpha one")
	send(t, c, "@beta two")
	f.reply("alpha", conversation.KindApproval, "ok?")
	f.reply("beta", conversation.KindApproval, "ok?")
	c.Tick(context.Background())

	sentBefore := len(f.sent)
	if got := send(t, c, "approve"); !strings.Contains(got.Text, "which one") || len(f.sent) != sentBefore {
		t.Fatalf("said %q, sent %v", got.Text, f.sent)
	}
}

func TestAtDesktopTargetsItAndRefusesWhenBusy(t *testing.T) {
	f := newFake(running("alpha"), running("beta"))
	c := newConcierge(t, f)

	send(t, c, "@Beta build the MIS report")
	if f.sent[0] != "beta: build the MIS report" {
		t.Fatalf("sent %v", f.sent)
	}
	got := send(t, c, "@beta another thing")
	if !strings.Contains(got.Text, "busy with #1") || len(f.sent) != 1 {
		t.Fatalf("said %q, sent %v", got.Text, f.sent)
	}
	if got := send(t, c, "@nobody hi"); !strings.Contains(got.Text, "don't know a desktop") {
		t.Fatalf("said %q", got.Text)
	}
}

func TestAtDesktopThatIsWaitingIsTakenAsTheReply(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "send the statements")
	f.reply("alpha", conversation.KindQuery, "Include disputed accounts?")
	c.Tick(context.Background())

	got := send(t, c, "@alpha no, skip them")
	if !strings.Contains(got.Text, "Sent your reply to #1") {
		t.Fatalf("said %q", got.Text)
	}
}

func TestAReplyToAJobNoLongerWaitingBecomesANewJob(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "first")
	f.reply("alpha", conversation.KindEntry, "done")
	c.Tick(context.Background())

	got := send(t, c, "#1 one more thing")
	if !strings.Contains(got.Text, "took that as a new job: #2") {
		t.Fatalf("said %q", got.Text)
	}
	if s, err := c.Store.Session(2); err != nil || s.Status != StatusRunning {
		t.Fatalf("session 2 = %+v, %v", s, err)
	}
}

func TestAStoppedDesktopEndsItsSession(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "long job")
	f.taskStatus[f.taskOf["alpha"]] = "abandoned"
	c.Tick(context.Background())

	if s, _ := c.Store.Session(1); s.Status != StatusStopped {
		t.Fatalf("status %s, want stopped", s.Status)
	}
	if got := lastSaid(t, c); !strings.Contains(got.Text, "ended without an answer") {
		t.Fatalf("said %q", got.Text)
	}
}

func TestStopCommand(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "long job")
	if got := send(t, c, "stop #1"); got.Text != "Stopped alpha." {
		t.Fatalf("said %q", got.Text)
	}
	if len(f.stopped) != 1 || f.stopped[0] != "alpha" {
		t.Fatalf("stopped %v", f.stopped)
	}
	if s, _ := c.Store.Session(1); s.Status != StatusStopped {
		t.Fatalf("status %s", s.Status)
	}
}

func TestStatusAndLiveStep(t *testing.T) {
	f := newFake(running("alpha"), running("beta"), desktop.Desktop{ID: "zzz", Name: "zzz", Status: "stopped"})
	c := newConcierge(t, f)

	send(t, c, "@alpha scrape prices")
	th := f.thread("alpha")
	th.Steps = []conversation.Step{{Text: "Opening the browser"}, {Text: "Reading gold price"}}

	got := send(t, c, "status")
	for _, want := range []string{"@alpha - working on #1", "@beta - free", "@zzz - stopped, free"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("status %q missing %q", got.Text, want)
		}
	}

	list, err := c.Sessions(10)
	if err != nil || len(list) != 1 || list[0].LiveStep != "Reading gold price" {
		t.Fatalf("sessions %+v, %v", list, err)
	}
}

func TestHelpAndEmpty(t *testing.T) {
	c := newConcierge(t, newFake())
	if got := send(t, c, "help"); !strings.Contains(got.Text, "@name <request>") {
		t.Fatalf("help %q", got.Text)
	}
	if _, err := c.Handle(context.Background(), "   "); err == nil {
		t.Fatal("empty message accepted")
	}
	if got := send(t, c, "do a thing"); !strings.Contains(got.Text, "no desktops yet") {
		t.Fatalf("said %q", got.Text)
	}
}

func TestMessagesAfterAndPrune(t *testing.T) {
	c := newConcierge(t, newFake())
	for i := 0; i < maxMessages+5; i++ {
		if _, err := c.Store.AddMessage(RoleConcierge, 0, "", "x"); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := c.Store.Messages(0, 5000)
	if len(all) != maxMessages {
		t.Fatalf("kept %d, want %d", len(all), maxMessages)
	}
	after, _ := c.Store.Messages(all[len(all)-3].ID, 100)
	if len(after) != 2 {
		t.Fatalf("after = %d, want 2", len(after))
	}
}
