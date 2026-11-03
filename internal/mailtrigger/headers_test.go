package mailtrigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
