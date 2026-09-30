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

	if err := c.Auth(smtp.PlainAuth("", user, password, host)); err != nil {
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

	return c.Quit()
}

// BuildMessage renders m as a MIME email from `from`: a plain-text body,
// and a multipart/mixed message when it has attachments.
func BuildMessage(from string, m OutgoingMail, at time.Time) ([]byte, error) {
	var buf bytes.Buffer

	header := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }

	header("From", from)
	header("To", strings.Join(m.To, ", "))
	if len(m.Cc) > 0 {
		header("Cc", strings.Join(m.Cc, ", "))
	}
	header("Subject", mime.QEncoding.Encode("utf-8", oneLine(m.Subject)))
	header("Date", at.Format(time.RFC1123Z))
	header("Message-ID", "<"+randomHex(12)+"@decurix>")
	header("MIME-Version", "1.0")

	if len(m.Attachments) == 0 {
		header("Content-Type", "text/plain; charset=utf-8")
		header("Content-Transfer-Encoding", "quoted-printable")
		buf.WriteString("\r\n")
		return buf.Bytes(), writeQP(&buf, m.Body)
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
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		name := mime.QEncoding.Encode("utf-8", a.Name)
		part, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {fmt.Sprintf("%s; name=%q", ct, name)},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename=%q", name)},
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
