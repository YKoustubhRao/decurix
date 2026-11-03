package httpapi

import (
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/keepawake"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// The email-trigger API (internal/mailtrigger): mailbox connections, the
// rules that turn a matching email into a request, and the jobs they queued.
// They are set up on the triggers page (beside schedules) and from the
// requests page's compose form. A job reaches its desktop through SubmitTracked, the same
// door a schedule's run takes - a fifth one onto submitLocked.
//
// Unlike most errors here, a mailbox's own error is shown as the server
// said it: "the password was refused" and "the server could not be reached"
// are fixed differently, and both are about the person's own mailbox.

// triggersPageHandler renders the triggers page: every way a request starts
// on its own - a schedule, a one-time run, an email - in one place.
func (s *Server) triggersPageHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/triggers" {
		http.NotFound(w, r)
		return
	}

	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		jsonError(w, err)
		return
	}

	if err := s.Web.RenderTriggers(w, webui.PageData{Desktops: desktops}); err != nil {
		log.Printf("triggers page: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// emailPageMovedHandler sends the old Email page's address to the email part
// of the triggers page, so a bookmark still lands on it.
func emailPageMovedHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/email" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/triggers#email", http.StatusMovedPermanently)
}

// mailStateHandler returns everything the page shows: the mailboxes (never
// their passwords), the rules, the latest jobs, the poller's status and
// whether this machine is being kept awake.
func (s *Server) mailStateHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	if !s.mailReady(w) {
		return
	}

	accounts, err := s.Mail.Accounts()
	if err != nil {
		jsonError(w, err)
		return
	}

	rules, err := s.Mail.Rules("")
	if err != nil {
		jsonError(w, err)
		return
	}

	jobs, err := s.Mail.Jobs(100)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{
		"accounts":   accounts,
		"rules":      rules,
		"jobs":       jobs,
		"poller":     s.MailRunner.Status(),
		"keep_awake": s.keepAwakeState(),
	})
}

// connectMailHandler tests a new mailbox connection and saves it only if the
// server accepted the login.
func (s *Server) connectMailHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))

	n := mailtrigger.NewAccount{
		Email:    r.FormValue("email"),
		Password: r.FormValue("password"),
		Host:     r.FormValue("host"),
		Port:     port,
		Folder:   r.FormValue("folder"),
	}

	if err := n.Normalise(); err != nil {
		mailInputError(w, err)
		return
	}

	validity, next, err := s.MailRunner.Connect(r.Context(), n.Account())
	if err != nil {
		mailConnectError(w, n.Host, err)
		return
	}

	acct, err := s.Mail.CreateAccount(n, validity, next)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, acct)
}

// mailPasswordHandler tests a new password for a saved mailbox (an app
// password that was revoked, say) and saves it only if it works.
func (s *Server) mailPasswordHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	acct, err := s.Mail.Account(r.FormValue("id"))
	if err != nil {
		mailStoreError(w, err)
		return
	}

	candidate, password, err := acct.WithPassword(r.FormValue("password"))
	if err != nil {
		mailInputError(w, err)
		return
	}

	if _, _, err := s.MailRunner.Connect(r.Context(), candidate); err != nil {
		mailConnectError(w, acct.Host, err)
		return
	}

	if err := s.Mail.UpdatePassword(acct.ID, password); err != nil {
		mailStoreError(w, err)
		return
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}

// toggleMailHandler pauses or resumes watching a mailbox.
func (s *Server) toggleMailHandler(w http.ResponseWriter, r *http.Request) {
	s.mailToggle(w, r, func(id string, enabled bool) error { return s.Mail.SetAccountEnabled(id, enabled) })
}

// checkMailHandler polls a mailbox now instead of waiting for its next poll.
func (s *Server) checkMailHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	if err := s.MailRunner.CheckNow(r.Context(), r.FormValue("id")); err != nil {
		if errors.Is(err, mailtrigger.ErrNotFound) {
			mailStoreError(w, err)
			return
		}

		log.Printf("mail: checking now: %v", err)
		jsonStatusError(w, http.StatusBadGateway, mailConnectReason(err))
		return
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}

// deleteMailHandler disconnects a mailbox: its saved password, rules and
// waiting jobs are deleted.
func (s *Server) deleteMailHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	if err := s.Mail.DeleteAccount(r.FormValue("id")); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// createMailRuleHandler saves a rule for a connected mailbox.
func (s *Server) createMailRuleHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	desktopID := desktop.SanitizeID(strings.TrimSpace(r.FormValue("desktop_id")))

	if desktopID != "" {
		if _, err := s.Desktops.Get(r.Context(), desktopID); err != nil {
			if errors.Is(err, desktop.ErrNotFound) {
				jsonStatusError(w, http.StatusBadRequest, "that desktop no longer exists")
				return
			}

			jsonError(w, err)
			return
		}
	}

	content := r.FormValue("content")
	if len(content) > maxRequestBytes {
		jsonStatusError(w, http.StatusBadRequest, "request too long")
		return
	}

	rule, err := s.Mail.CreateRule(mailtrigger.NewRule{
		AccountID:    r.FormValue("account_id"),
		DesktopID:    desktopID,
		Name:         r.FormValue("name"),
		SenderRe:     r.FormValue("sender_re"),
		SubjectRe:    r.FormValue("subject_re"),
		Content:      content,
		IncludeBody:  r.FormValue("include_body") == "true",
		VerifiedOnly: r.FormValue("verified_only") == "true",
	})
	if err != nil {
		mailInputError(w, err)
		return
	}

	jsonResponse(w, rule)
}

// toggleMailRuleHandler pauses or resumes a rule.
func (s *Server) toggleMailRuleHandler(w http.ResponseWriter, r *http.Request) {
	s.mailToggle(w, r, func(id string, enabled bool) error { return s.Mail.SetRuleEnabled(id, enabled) })
}

// deleteMailRuleHandler removes a rule and its waiting jobs.
func (s *Server) deleteMailRuleHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	if err := s.Mail.DeleteRule(r.FormValue("id")); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// testMailRuleHandler shows which of a mailbox's latest emails a sender and
// subject pattern would match, without queueing anything.
func (s *Server) testMailRuleHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	results, err := s.MailRunner.Test(r.Context(), r.FormValue("account_id"), r.FormValue("sender_re"), r.FormValue("subject_re"), 20)
	if err != nil {
		if errors.Is(err, mailtrigger.ErrNotFound) {
			mailStoreError(w, err)
			return
		}

		var input mailtrigger.InputError
		if errors.As(err, &input) {
			mailInputError(w, err)
			return
		}

		log.Printf("mail: testing a rule: %v", err)
		jsonStatusError(w, http.StatusBadGateway, mailConnectReason(err))
		return
	}

	jsonResponse(w, results)
}

// mailJobHandler retries or cancels one job (?action=retry|cancel).
func (s *Server) mailJobHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	var err error

	switch r.FormValue("action") {
	case "retry":
		err = s.Mail.RetryJob(r.FormValue("id"))
	case "cancel":
		err = s.Mail.CancelJob(r.FormValue("id"))
	default:
		jsonStatusError(w, http.StatusBadRequest, "action must be retry or cancel")
		return
	}

	if err != nil {
		if errors.Is(err, mailtrigger.ErrNotFound) {
			jsonStatusError(w, http.StatusConflict, "that job is not in a state it can be moved from")
			return
		}

		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}

// keepAwakeHandler says whether this machine should be kept from sleeping -
// what a host script polls when the control plane runs in a container, where
// it cannot hold the request itself (see internal/keepawake).
func (s *Server) keepAwakeHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	// ?holder=script is a host script asking so it can hold the machine
	// awake; remembering when it last did is how the page knows whether
	// anything is (keepawake.State.Script).
	if r.URL.Query().Get("holder") == "script" {
		s.keepAwakeScriptAt.Store(time.Now().UnixNano())
	}

	jsonResponse(w, s.keepAwakeState())
}

func (s *Server) keepAwakeState() keepawake.State {
	state := keepawake.State{Enabled: keepawake.Enabled(), Reasons: []string{}}
	if s.KeepAwake != nil {
		state = s.KeepAwake.State()
	}

	if at := s.keepAwakeScriptAt.Load(); at != 0 {
		state.Script = time.Since(time.Unix(0, at)) < keepawake.ScriptFresh
	}

	return state
}

func (s *Server) mailReady(w http.ResponseWriter) bool {
	if s.Mail == nil || s.MailRunner == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "email triggers are not set up")
		return false
	}

	return true
}

func mailStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, mailtrigger.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "not found")
		return
	}

	jsonError(w, err)
}

// mailConnectError says why a mailbox could not be connected, in words that
// say what to do about it.
func mailConnectError(w http.ResponseWriter, host string, err error) {
	if mailtrigger.IsAuth(err) {
		msg := "The mail server refused the login."
		if host == "imap.gmail.com" {
			msg = "Gmail refused the login. Use an app password, not your Google password: " +
				"Google Account > Security > 2-Step Verification > App passwords."
		}

		// err reads "<ErrAuth> (<the server's words>)"; keep only the words.
		said := strings.TrimSpace(strings.TrimPrefix(err.Error(), mailtrigger.ErrAuth.Error()))
		jsonStatusError(w, http.StatusBadRequest, strings.TrimSpace(msg+" "+said))
		return
	}

	log.Printf("mail: connecting to %s: %v", host, err)
	jsonStatusError(w, http.StatusBadGateway, mailConnectReason(err))
}

// mailInputError answers a mistake in what the person gave with its own
// words, and any other error as internal: an error's text is shown only when
// it was written for the person (review 2026-11-03, B-37).
func mailInputError(w http.ResponseWriter, err error) {
	var input mailtrigger.InputError
	if errors.As(err, &input) {
		jsonStatusError(w, http.StatusBadRequest, input.Error())
		return
	}

	jsonError(w, err)
}

// mailConnectReason says, in words chosen here, why a mail server could not
// be used: the error itself is logged, never shown.
func mailConnectReason(err error) string {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var netErr net.Error

	switch {
	case mailtrigger.IsAuth(err):
		return "The mail server refused the login."
	case errors.As(err, &dns):
		return "Could not connect: the mail server's name was not found. Check the server name."
	case errors.As(err, &cert):
		return "Could not connect: the mail server's certificate could not be verified."
	case errors.Is(err, syscall.ECONNREFUSED):
		return "Could not connect: the mail server refused the connection. Check the server name and port."
	case errors.As(err, &netErr) && netErr.Timeout():
		return "Could not connect: the mail server did not answer in time."
	case errors.Is(err, mailtrigger.ErrPasswordUnreadable):
		return "The saved password can no longer be read; type it again."
	default:
		return "Could not connect to the mail server. The control plane's log says why."
	}
}

// mailToggle is the shared body of the mailbox and rule pause/resume
// handlers: set is called with the form's id and enabled.
func (s *Server) mailToggle(w http.ResponseWriter, r *http.Request, set func(id string, enabled bool) error) {
	if !requirePost(w, r) || !s.mailReady(w) {
		return
	}

	enabled, err := strconv.ParseBool(r.FormValue("enabled"))
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "enabled must be true or false")
		return
	}

	if err := set(r.FormValue("id"), enabled); err != nil {
		mailStoreError(w, err)
		return
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}
