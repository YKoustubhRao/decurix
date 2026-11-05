package mailtrigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// badHeaderSession fails any header fetch that includes one email, as a server that chokes on it does.
type badHeaderSession struct {
	fakeSession
	bad uint32
}

func (s badHeaderSession) Headers(uids []uint32) (map[uint32][]byte, error) {
	for _, u := range uids {
		if u == s.bad {
			return nil, errors.New("NO the server could not fetch a message")
		}
	}
	return s.fakeSession.Headers(uids)
}

// An email whose headers the server will not return is counted and, after maxReadTries polls, passed over: the email
// after it is handled and the mailbox says why one was skipped (RW1-B-01).
func TestAnEmailWhoseHeadersFailIsPassedOver(t *testing.T) {
	h := newHarness(t)
	h.rule(t, NewRule{SubjectRe: `.`})
	bad := h.box.deliver("a@x.com", "Broken", "x")
	h.box.deliver("a@x.com", "Next", "y")
	h.runner.Dial = func(ctx context.Context, acct Account) (Mailbox, error) {
		mb, err := h.box.dialer(ctx, acct)
		if err != nil {
			return nil, err
		}
		return badHeaderSession{mb.(fakeSession), bad}, nil
	}
	for i := 0; i < maxReadTries+2; i++ {
		h.advance(20 * time.Minute)
		h.runner.step(context.Background())
	}
	acct, _ := h.store.Account(h.acct.ID)
	if acct.lastUID <= bad || len(h.jobs(t)) != 1 {
		t.Fatalf("lastUID=%d (bad %d), %d jobs: the mailbox is stuck behind the broken email", acct.lastUID, bad, len(h.jobs(t)))
	}
}

// linkDownSession loses the connection whenever it reads one email's text, as a flaky link does.
type linkDownSession struct {
	fakeSession
	bad uint32
	err error
}

func (s linkDownSession) Text(uid uint32) ([]byte, error) {
	if uid == s.bad {
		return nil, s.err
	}
	return s.fakeSession.Text(uid)
}

// A read that fails because the link to the server failed (a reset connection, a timeout) is not the email's fault:
// it never counts against the email's tries, so a good email is not passed over unread however many polls the link
// fails (REVIEW-3 follow-up 5).
func TestALinkFailureNeverPassesAnEmailOver(t *testing.T) {
	for name, linkErr := range map[string]error{
		"reset":    fmt.Errorf("reading from the mail server: %w", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}),
		"cut":      fmt.Errorf("reading from the mail server: %w", io.ErrUnexpectedEOF),
		"deadline": fmt.Errorf("reading from the mail server: %w", os.ErrDeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.rule(t, NewRule{SubjectRe: `.`, IncludeBody: true})
			bad := h.box.deliver("a@x.com", "Good one", "x")
			h.runner.Dial = func(ctx context.Context, acct Account) (Mailbox, error) {
				mb, err := h.box.dialer(ctx, acct)
				if err != nil {
					return nil, err
				}
				return linkDownSession{mb.(fakeSession), bad, linkErr}, nil
			}
			for i := 0; i < maxReadTries+2; i++ {
				h.advance(3 * time.Hour)
				h.runner.step(context.Background())
			}
			acct, _ := h.store.Account(h.acct.ID)
			if acct.lastUID >= bad || acct.stuckTries != 0 || len(h.jobs(t)) != 0 {
				t.Fatalf("lastUID=%d (email %d), tries %d, %d jobs: a link failure passed the email over", acct.lastUID, bad, acct.stuckTries, len(h.jobs(t)))
			}
		})
	}
}

// An email that cannot be read within the poll's session deadline (a large scan on a slow link) fails every poll on
// the link, never on the email: after maxLinkTries polls in a row it is passed over with a failed job a person sees,
// and the email after it is handled (RW1-G-01, the reviewer's scenario).
func TestAnEmailTheLinkNeverReadsIsPassedOverInTheEnd(t *testing.T) {
	h := newHarness(t)
	h.rule(t, NewRule{SubjectRe: `.`, IncludeBody: true})
	big := h.box.deliver("a@x.com", "Scan 20 MB", "x")
	h.box.deliver("a@x.com", "Next", "y")
	h.runner.Dial = func(ctx context.Context, acct Account) (Mailbox, error) {
		mb, err := h.box.dialer(ctx, acct)
		if err != nil {
			return nil, err
		}
		return linkDownSession{mb.(fakeSession), big, fmt.Errorf("reading from the mail server: %w", os.ErrDeadlineExceeded)}, nil
	}
	for i := 0; i < maxLinkTries-1; i++ {
		h.advance(6 * time.Hour)
		h.runner.step(context.Background())
	}
	if acct, _ := h.store.Account(h.acct.ID); acct.lastUID >= big || len(h.jobs(t)) != 0 {
		t.Fatalf("passed over before maxLinkTries: lastUID=%d, %d jobs", acct.lastUID, len(h.jobs(t)))
	}
	for i := 0; i < 2; i++ {
		h.advance(6 * time.Hour)
		h.runner.step(context.Background())
	}
	var failed, sent int
	for _, j := range h.jobs(t) {
		switch {
		case j.Subject == "Scan 20 MB" && j.Status == JobFailed && strings.Contains(j.Note, "connection to the mail server failed"):
			failed++
		case j.Subject == "Next" && j.Status == JobSent:
			sent++
		}
	}
	if failed != 1 || sent != 1 {
		t.Fatalf("want the unreadable email failed with a note and the next one sent; jobs = %+v", h.jobs(t))
	}
	if acct, _ := h.store.Account(h.acct.ID); acct.stuckUID != 0 || acct.stuckLinkTries != 0 {
		t.Fatalf("the mailbox is still marked stuck: %d, %d", acct.stuckUID, acct.stuckLinkTries)
	}
}
