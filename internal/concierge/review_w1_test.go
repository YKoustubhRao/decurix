package concierge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A reply whose final check did not judge it is "finished, not confirmed", never done (W-01, C-15).
func TestAnUncheckedReplyIsNotDone(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "file the return")
	no := false
	f.outcome[f.taskOf["alpha"]] = tasks.DisplayOutcome(tasks.StatusDone, &no)
	f.reply("alpha", conversation.KindEntry, "Filed.")
	c.Tick(context.Background())

	s, _ := c.Store.Session(1)
	if s.Status != StatusUnchecked {
		t.Fatalf("status = %s, want %s", s.Status, StatusUnchecked)
	}
	if got := lastSaid(t, c); got.Kind != KindUnchecked {
		t.Fatalf("relayed as %q, want %q", got.Kind, KindUnchecked)
	}
	if !strings.Contains(c.sessionsText(), "finished, not confirmed") {
		t.Fatalf("sessions say %q", c.sessionsText())
	}
}

// Without the task record to ask, a reply is not called done either.
func TestAReplyWithNoRecordToAskIsNotDone(t *testing.T) {
	f := newFake(running("alpha"))
	deps := f.deps()
	deps.TaskOutcome = nil
	c := newConcierge(t, f)
	c.Deps = deps

	send(t, c, "count the rows")
	f.reply("alpha", conversation.KindEntry, "12")
	c.Tick(context.Background())

	if s, _ := c.Store.Session(1); s.Status == StatusDone {
		t.Fatal("a reply nobody's check judged was called done")
	}
}

// A bare "ok" with one approval waiting is a message to the concierge, never that approval's answer (C-15).
func TestABareOkNeverAnswersAnApproval(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "pay the invoice")
	f.reply("alpha", conversation.KindApproval, "pay 400 to the vendor?")
	c.Tick(context.Background())

	sent := len(f.sent)
	for _, word := range []string{"ok", "sure", "go", "yes"} {
		got := send(t, c, word)
		if len(f.sent) != sent {
			t.Fatalf("%q was sent to the desktop: %v", word, f.sent)
		}
		if !strings.Contains(got.Text, "Approve") {
			t.Fatalf("%q answered %q, want a pointer to the Approve button", word, got.Text)
		}
	}
}

// The Approve button answers the approval with the context the server bound to it; a typed "#N yes" goes as a
// message and the person is told it approves nothing.
func TestApproveButtonAnswersAndTypedYesIsOnlyAMessage(t *testing.T) {
	f := newFake(running("alpha"))
	var bound []bool
	deps := f.deps()
	send0 := deps.Send
	deps.Send = func(ctx context.Context, id, text string) (string, string, error) {
		bound = append(bound, ctx.Value(testNoteKey{}) == "note-1")
		return send0(ctx, id, text)
	}
	c := newConcierge(t, f)
	c.Deps = deps

	send(t, c, "pay the invoice")
	f.reply("alpha", conversation.KindApproval, "pay 400 to the vendor?")
	c.Tick(context.Background())

	got := send(t, c, "#1 yes")
	if !strings.Contains(got.Text, "approves nothing") {
		t.Fatalf("typed yes said %q", got.Text)
	}

	// The desktop asks again; the person uses the button.
	f.reply("alpha", conversation.KindApproval, "pay 400 to the vendor?")
	c.Tick(context.Background())
	ctx := context.WithValue(context.Background(), testNoteKey{}, "note-1")
	if err := c.Answer(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if f.sent[len(f.sent)-1] != "alpha: yes" || len(bound) != 3 || !bound[2] || bound[1] {
		t.Fatalf("sent %v, bound %v", f.sent, bound)
	}
	if strings.Contains(lastSaid(t, c).Text, "approves nothing") {
		t.Fatal("a bound answer was told it approves nothing")
	}

	// A job not waiting on an approval is not answered by a button.
	if err := c.Answer(ctx, 1, true); !errors.Is(err, ErrNotWaitingApproval) {
		t.Fatalf("Answer on a running job = %v", err)
	}
}

type testNoteKey struct{}

// What a person types is stored with its secrets withheld; a reply to a question asked for a secret is stored
// withheld whole - and still sent to the desktop as typed (C-04, X-6).
func TestTheConciergeKeepsNoSecretAsTyped(t *testing.T) {
	f := newFake(running("alpha"))
	c := newConcierge(t, f)

	send(t, c, "log in to the portal with password Xy7#q2")
	msgs, _ := c.Store.Messages(0, 100)
	if strings.Contains(msgs[0].Text, "Xy7#q2") {
		t.Fatalf("stored %q", msgs[0].Text)
	}
	if sess, _ := c.Store.Session(1); strings.Contains(sess.Request, "Xy7#q2") {
		t.Fatalf("session request %q", sess.Request)
	}
	if !strings.Contains(f.sent[0], "Xy7#q2") {
		t.Fatalf("the desktop did not get the request as typed: %v", f.sent)
	}

	f.reply("alpha", conversation.KindQuery, "Which code did you get?")
	f.questions[f.taskOf["alpha"]] = fakeQuestion{text: "Which code did you get?", secret: true}
	c.Tick(context.Background())

	got, _ := c.Handle(context.Background(), "#1 482913")
	if strings.Contains(got.Text, "482913") || !strings.Contains(got.Text, withhold.Secret) {
		t.Fatalf("stored the reply as %q", got.Text)
	}
	if f.sent[len(f.sent)-1] != "alpha: 482913" {
		t.Fatalf("the desktop did not get the reply as typed: %v", f.sent)
	}
}
