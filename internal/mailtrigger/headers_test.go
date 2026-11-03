package mailtrigger

import (
	"context"
	"errors"
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
