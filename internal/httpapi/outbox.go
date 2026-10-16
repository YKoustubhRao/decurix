package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/outbox"
)

// The Outbox: emails an agent prepared (the azure harness's draft_email),
// waiting for a person to send them. Creating one is for the desktops, over
// the primary listener (RegisterLocalRoutes); listing, sending and
// discarding are humansOnly - an agent, or a prompt injected into one, must
// never be able to press Send on its own draft.

// outboxCreateHandler keeps a draft sent by a desktop's agent.
func (s *Server) outboxCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.Outbox == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "the outbox is not available")
		return
	}

	var in struct {
		DesktopID   string   `json:"desktop_id"`
		TaskID      string   `json:"task_id"`
		To          []string `json:"to"`
		Cc          []string `json:"cc"`
		Subject     string   `json:"subject"`
		Body        string   `json:"body"`
		Replaces    string   `json:"replaces"`
		Attachments []struct {
			Name        string `json:"name"`
			ContentType string `json:"content_type"`
			Data        string `json:"data"`
		} `json:"attachments"`
	}

	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&in); err != nil {
		jsonStatusError(w, http.StatusBadRequest, "the draft is not valid JSON")
		return
	}

	d := outbox.Draft{
		DesktopID: desktop.SanitizeID(in.DesktopID),
		TaskID:    in.TaskID,
		To:        in.To,
		Cc:        in.Cc,
		Subject:   in.Subject,
		Body:      in.Body,
	}

	for _, a := range in.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			jsonStatusError(w, http.StatusBadRequest, "an attachment is not base64")
			return
		}
		d.Files = append(d.Files, outbox.File{Name: mailtrigger.SafeFileName(a.Name), ContentType: a.ContentType, Data: data})
	}

	// A draft may replace one the same task of the same desktop made earlier and nobody has sent: a worker
	// told its first attempt was wrong could otherwise only add a second copy of every email (found live,
	// 2026-10-13: seven dunning letters drafted twice). Nothing a person sent, and nothing another task
	// drafted, can be withdrawn this way.
	var old outbox.Draft
	if in.Replaces != "" {
		var err error
		old, err = s.Outbox.Get(in.Replaces)
		switch {
		case errors.Is(err, outbox.ErrNotFound):
			jsonStatusError(w, http.StatusNotFound, "there is no draft "+in.Replaces+" to replace")
			return
		case err != nil:
			jsonError(w, err)
			return
		case old.DesktopID != d.DesktopID || in.TaskID == "" || old.TaskID != in.TaskID:
			jsonStatusError(w, http.StatusConflict, "only a draft this same task made can be replaced")
			return
		case old.Status != outbox.StatusDraft:
			jsonStatusError(w, http.StatusConflict, "that email was already sent or discarded; it cannot be replaced")
			return
		}
	}

	saved, err := s.Outbox.Add(d)
	if errors.Is(err, outbox.ErrInvalid) {
		jsonStatusError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "outbox: "))
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	// Discarded only once its replacement is kept, so a failure leaves two drafts, never none.
	replaced := ""
	if old.ID != "" {
		if err := s.Outbox.Discard(old.ID); err == nil {
			replaced = old.ID
		} else {
			log.Printf("outbox: draft %s was kept but %s it replaces could not be discarded: %v", saved.ID, old.ID, err)
		}
	}

	jsonResponse(w, map[string]any{"id": saved.ID, "status": saved.Status, "replaced": replaced})
}

// outboxListHandler lists drafts and sent emails, newest first, with the
// mailboxes they can be sent from.
func (s *Server) outboxListHandler(w http.ResponseWriter, r *http.Request) {
	if s.Outbox == nil {
		jsonResponse(w, map[string]any{"drafts": []any{}, "accounts": []any{}})
		return
	}

	drafts, err := s.Outbox.List(200)
	if err != nil {
		jsonError(w, err)
		return
	}

	type account struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}

	accounts := []account{}
	if s.Mail != nil {
		accts, err := s.Mail.Accounts()
		if err != nil {
			jsonError(w, err)
			return
		}
		for _, a := range accts {
			accounts = append(accounts, account{ID: a.ID, Email: a.Email})
		}
	}

	if drafts == nil {
		drafts = []outbox.Draft{}
	}

	jsonResponse(w, map[string]any{"drafts": drafts, "accounts": accounts})
}

// outboxFileHandler downloads one attachment of a draft, for a person to
// check what is about to go out.
func (s *Server) outboxFileHandler(w http.ResponseWriter, r *http.Request) {
	if s.Outbox == nil {
		http.NotFound(w, r)
		return
	}

	d, err := s.Outbox.Get(r.URL.Query().Get("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	name := r.URL.Query().Get("name")
	for _, f := range d.Files {
		if f.Name == name {
			ct := f.ContentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(f.Data)
			return
		}
	}

	http.NotFound(w, r)
}

// outboxSendHandler sends a draft from a connected mailbox. id, account_id;
// optionally to/cc/subject/body when the person edited them first.
func (s *Server) outboxSendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.Outbox == nil || s.Mail == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "connect a mailbox on the Triggers page first")
		return
	}

	accountID := r.FormValue("account_id")
	if accountID == "" {
		jsonStatusError(w, http.StatusBadRequest, "choose the mailbox to send from")
		return
	}

	d, err := s.Outbox.Claim(r.FormValue("id"))
	switch {
	case errors.Is(err, outbox.ErrNotFound):
		jsonStatusError(w, http.StatusNotFound, "no such email")
		return
	case errors.Is(err, outbox.ErrNotDraft):
		jsonStatusError(w, http.StatusConflict, "that email was already sent or discarded")
		return
	case err != nil:
		jsonError(w, err)
		return
	}

	m := mailtrigger.OutgoingMail{To: d.To, Cc: d.Cc, Subject: d.Subject, Body: d.Body}
	if v := strings.TrimSpace(r.FormValue("to")); v != "" {
		m.To = splitAddrs(v)
	}
	if _, ok := r.Form["cc"]; ok {
		m.Cc = splitAddrs(r.FormValue("cc"))
	}
	if _, ok := r.Form["subject"]; ok {
		m.Subject = r.FormValue("subject")
	}
	if _, ok := r.Form["body"]; ok {
		m.Body = r.FormValue("body")
	}

	for _, f := range d.Files {
		m.Attachments = append(m.Attachments, mailtrigger.Attachment{Name: f.Name, ContentType: f.ContentType, Data: f.Data})
	}

	sendErr := s.Mail.SendMail(r.Context(), accountID, m)
	if err := s.Outbox.Finish(d.ID, sendErr); err != nil {
		log.Printf("outbox: recording %s: %v", d.ID, err)
	}

	if sendErr != nil {
		log.Printf("outbox: sending %s: %v", d.ID, sendErr)
		// The person pressed Send and needs to know why it did not go; the
		// reason is ours (mailtrigger wraps every failure) and this route is
		// humansOnly.
		jsonResponse(w, map[string]any{"status": outbox.StatusFailed, "note": sendErr.Error()})
		return
	}

	jsonResponse(w, map[string]any{"status": outbox.StatusSent})
}

// outboxDiscardHandler drops a draft.
func (s *Server) outboxDiscardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || s.Outbox == nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := s.Outbox.Discard(r.FormValue("id"))
	switch {
	case errors.Is(err, outbox.ErrNotFound):
		jsonStatusError(w, http.StatusNotFound, "no such email")
	case errors.Is(err, outbox.ErrNotDraft):
		jsonStatusError(w, http.StatusConflict, "that email was already sent or discarded")
	case err != nil:
		jsonError(w, err)
	default:
		jsonResponse(w, map[string]any{"status": outbox.StatusDiscarded})
	}
}

func splitAddrs(v string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
