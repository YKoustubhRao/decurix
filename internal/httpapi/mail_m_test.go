package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
)

// A mistake in what the person typed is answered in its own words; an
// internal or server error never is (review 2026-11-03, B-37).
func TestMailErrorsShowOnlyWhatWasWrittenForThePerson(t *testing.T) {
	s := mailServer(t)

	rec := postForm(t, s.connectMailHandler, "/api/mail/accounts/connect", "email=nope&password=x")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "enter the email address") {
		t.Fatalf("a typing mistake: %d %s", rec.Code, rec.Body)
	}

	s.MailRunner.Dial = func(context.Context, mailtrigger.Account) (mailtrigger.Mailbox, error) {
		return nil, errors.New("dial tcp 10.1.2.3:993: internal detail")
	}
	rec = postForm(t, s.connectMailHandler, "/api/mail/accounts/connect", "email=me%40example.com&password=x&host=imap.example.com")
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "internal detail") || strings.Contains(rec.Body.String(), "10.1.2.3") {
		t.Fatalf("a connection error was echoed: %d %s", rec.Code, rec.Body)
	}
}

func TestScheduleErrorsShowOnlyWhatWasWrittenForThePerson(t *testing.T) {
	rec := createSchedule(t, "id=bordu&content=x&cron=61+*+*+*+*", http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "minute field") {
		t.Fatalf("a cron mistake was not explained: %s", rec.Body)
	}
}
