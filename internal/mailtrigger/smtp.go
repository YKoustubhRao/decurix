package mailtrigger

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sending mail from a connected mailbox, for the Outbox (internal/outbox):
// an agent drafts an email, a person presses Send, and it leaves through the
// same account the person connected for email triggers, with the same app
// password. It lives here because that password never leaves this package.

// Attachment is one file of an outgoing email.
type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

// OutgoingMail is an email to send.
type OutgoingMail struct {
	To          []string
	Cc          []string
	Subject     string
	Body        string
	Attachments []Attachment
}

// ErrNoRecipients is an email with nobody to send it to.
var ErrNoRecipients = errors.New("mailtrigger: the email has no recipients")

// smtpAddr is where a mailbox's mail is sent: DECURIX_SMTP_HOST/PORT when
// set, else the IMAP host with "imap." made "smtp." (imap.gmail.com ->
// smtp.gmail.com, imap.mail.yahoo.com -> smtp.mail.yahoo.com) on 465.
func smtpAddr(acct Account) (string, int) {
	host := strings.TrimSpace(os.Getenv("DECURIX_SMTP_HOST"))
	if host == "" {
		host = acct.Host
		if strings.HasPrefix(host, "imap.") {
			host = "smtp." + strings.TrimPrefix(host, "imap.")
		} else if strings.HasPrefix(host, "outlook.office365.com") {
			host = "smtp.office365.com"
		}
	}

	port := 465
	if p, err := strconv.Atoi(os.Getenv("DECURIX_SMTP_PORT")); err == nil && p > 0 {
		port = p
	} else if host == "smtp.office365.com" {
		port = 587
	}

	return host, port
}

// SendMail sends m from the mailbox accountID, over implicit TLS (465) or
// STARTTLS (587), logging in with the mailbox's own address and password.
func (s *Store) SendMail(ctx context.Context, accountID string, m OutgoingMail) error {
	acct, err := s.Account(accountID)
	if err != nil {
		return err
	}

	if acct.passwordErr != nil || acct.password == "" {
		return fmt.Errorf("mailtrigger: the mailbox %s needs its password again", acct.Email)
	}

	var rcpt []string
	for _, a := range append(append([]string{}, m.To...), m.Cc...) {
		parsed, err := mail.ParseAddress(strings.TrimSpace(a))
		if err != nil {
			return fmt.Errorf("mailtrigger: %q is not an email address", a)
		}
		rcpt = append(rcpt, parsed.Address)
	}

	if len(rcpt) == 0 {
		return ErrNoRecipients
	}

	msg, err := BuildMessage(acct.Email, m, time.Now())
	if err != nil {
		return err
	}

	host, port := smtpAddr(acct)

	return sendSMTP(ctx, host, port, acct.Email, acct.password, rcpt, msg)
}

func sendSMTP(ctx context.Context, host string, port int, user, password string, rcpt []string, msg []byte) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 20 * time.Second}

	var conn net.Conn
	var err error

	if port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}

	if err != nil {
		return fmt.Errorf("mailtrigger: connecting to %s: %w", addr, err)
	}

	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mailtrigger: %s: %w", addr, err)
	}
	defer c.Close()

	if port != 465 {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("mailtrigger: %s offers no TLS; refusing to send a password in the clear", addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("mailtrigger: STARTTLS with %s: %w", addr, err)
		}
	}

	return converse(c, smtp.PlainAuth("", user, password, host), user, rcpt, msg)
}

// smtpConversation is the part of *smtp.Client a send uses once the
// connection is secured.
type smtpConversation interface {
	Auth(smtp.Auth) error
	Mail(from string) error
	Rcpt(to string) error
	Data() (io.WriteCloser, error)
	Quit() error
}

// converse logs in and hands the server one email. Once the server has
// accepted the message (its reply to the end of DATA), the email is sent: a
// failed goodbye after that is logged, never returned, since a returned
// error leaves the draft to be sent again and the email would go out twice
// (review 2026-11-03, B-28).
func converse(c smtpConversation, auth smtp.Auth, user string, rcpt []string, msg []byte) error {
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("mailtrigger: the mail server refused the login: %w", err)
	}

	if err := c.Mail(user); err != nil {
		return fmt.Errorf("mailtrigger: %w", err)
	}

	for _, r := range rcpt {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("mailtrigger: the server refused %s: %w", r, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailtrigger: %w", err)
	}

	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("mailtrigger: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("mailtrigger: the server did not accept the email: %w", err)
	}

	if err := c.Quit(); err != nil {
		log.Printf("mailtrigger: the server accepted the email, then the goodbye failed (the email is sent): %v", err)
	}

	return nil
}

// BuildMessage renders m as a MIME email from `from`: a plain-text body,
// and a multipart/mixed message when it has attachments.
func BuildMessage(from string, m OutgoingMail, at time.Time) ([]byte, error) {
	var buf bytes.Buffer

	header := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }

	// Every header value is built from parsed parts, never pasted in: a draft's
	// recipients and file types come from an agent, and a line break in one
	// would start a header of its own (review 2026-11-03, B-16).
	if strings.ContainsAny(from, "\r\n") {
		return nil, fmt.Errorf("mailtrigger: the sending address has a line break in it")
	}

	to, err := headerAddresses(m.To)
	if err != nil {
		return nil, err
	}

	cc, err := headerAddresses(m.Cc)
	if err != nil {
		return nil, err
	}

	header("From", from)
	header("To", to)
	if cc != "" {
		header("Cc", cc)
	}
	header("Subject", mime.QEncoding.Encode("utf-8", oneLine(m.Subject)))
	header("Date", at.Format(time.RFC1123Z))
	header("Message-ID", "<"+randomHex(12)+"@decurix>")
	header("MIME-Version", "1.0")

	if len(m.Attachments) == 0 {
		header("Content-Type", "text/plain; charset=utf-8")
		header("Content-Transfer-Encoding", "quoted-printable")
		buf.WriteString("\r\n")
		if err := writeQP(&buf, m.Body); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	mw := multipart.NewWriter(&buf)
	header("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	buf.WriteString("\r\n")

	bodyPart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	if err := writeQP(bodyPart, m.Body); err != nil {
		return nil, err
	}

	for _, a := range m.Attachments {
		name := strings.Map(dropControl, a.Name)
		part, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mediaTypeWithName(a.ContentType, name)},
			"Content-Disposition":       {formatMediaType("attachment", name, "filename")},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return nil, err
		}

		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			fmt.Fprintf(part, "%s\r\n", enc[:76])
			enc = enc[76:]
		}
		fmt.Fprintf(part, "%s\r\n", enc)
	}

	if err := mw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// headerAddresses renders addresses for a To or Cc header from their parsed
// form, refusing any with a line break (net/mail accepts one inside a
// comment, and a header pasted from the raw text would then be split).
func headerAddresses(list []string) (string, error) {
	out := make([]string, 0, len(list))
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if strings.ContainsAny(raw, "\r\n") {
			return "", fmt.Errorf("mailtrigger: the address %q has a line break in it", clip(strings.Map(dropControl, raw)))
		}
		addr, err := mail.ParseAddress(raw)
		if err != nil {
			return "", fmt.Errorf("mailtrigger: %q is not an email address", clip(raw))
		}
		out = append(out, addr.String())
	}
	return strings.Join(out, ", "), nil
}

// mediaTypeWithName is a part's Content-Type re-rendered from its parsed form,
// with the file's name: a type that does not parse is sent as
// application/octet-stream.
func mediaTypeWithName(contentType, name string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || strings.ContainsAny(contentType, "\r\n") {
		mediaType = "application/octet-stream"
	}
	return formatMediaType(mediaType, name, "name")
}

// formatMediaType renders "mediaType; param=name", the name encoded as RFC
// 2231 when it needs to be.
func formatMediaType(mediaType, name, param string) string {
	if out := mime.FormatMediaType(mediaType, map[string]string{param: name}); out != "" {
		return out
	}
	return mime.FormatMediaType(mediaType, map[string]string{param: "file"})
}

// dropControl drops control characters from a header value.
func dropControl(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return -1
	}
	return r
}

func writeQP(w interface{ Write([]byte) (int, error) }, text string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n"))); err != nil {
		return err
	}
	return qp.Close()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
