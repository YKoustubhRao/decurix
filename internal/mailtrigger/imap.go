package mailtrigger

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A minimal IMAP4rev1 client: exactly the commands a poll needs (LOGIN,
// EXAMINE, UID SEARCH, UID FETCH, LOGOUT), over implicit TLS. Hand-written
// rather than a library for the reason internal/schedule's cron parser and
// httpapi's WebSocket are: the binary stays static and dependency-light.
// Everything is read-only - EXAMINE, never SELECT, and BODY.PEEK, never BODY -
// so polling never marks a message as read in the person's own mail client.

// sessionTimeout bounds one whole poll's connection, so a server that stops
// answering (or a socket left half-open by the laptop sleeping mid-poll)
// cannot hold a poll open forever.
const sessionTimeout = 90 * time.Second

// bodyBytes caps how much of a message's body is fetched: only its start is
// ever put into a request.
const bodyBytes = 64 * 1024

// ErrAuth is a login the server refused - a wrong or revoked app password,
// rather than a network problem. The page says which, since only one of them
// is fixed by typing the password again.
var ErrAuth = errors.New("the mail server refused the login")

// Mailbox is one open, logged-in session on one folder. Satisfied by the IMAP
// client below; narrowed to an interface so the Runner is tested against a
// fake mailbox instead of a server.
type Mailbox interface {
	// Examine opens folder read-only and reports its UIDVALIDITY and the UID
	// the next message delivered to it will get.
	Examine(folder string) (uidValidity, uidNext uint32, err error)
	// UIDsAfter lists the UIDs above after, ascending.
	UIDsAfter(after uint32) ([]uint32, error)
	// Headers fetches the header block of each UID.
	Headers(uids []uint32) (map[uint32][]byte, error)
	// Text fetches the start of a message's body (everything after its header).
	Text(uid uint32) ([]byte, error)
	// Close logs out and closes the connection.
	Close()
}

// Dialer opens a Mailbox for an account.
type Dialer func(ctx context.Context, acct Account) (Mailbox, error)

// DialIMAP connects to acct's server over TLS and logs in.
func DialIMAP(ctx context.Context, acct Account) (Mailbox, error) {
	addr := net.JoinHostPort(acct.Host, strconv.Itoa(acct.Port))

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 20 * time.Second},
		Config:    &tls.Config{ServerName: acct.Host, MinVersion: tls.VersionTLS12},
	}

	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", addr, err)
	}

	c := newIMAPClient(raw)

	if err := c.greeting(); err != nil {
		raw.Close()
		return nil, err
	}

	if err := c.login(acct.Email, acct.password); err != nil {
		raw.Close()
		return nil, err
	}

	return c, nil
}

type imapClient struct {
	conn net.Conn
	r    *bufio.Reader
	tag  int
}

func newIMAPClient(conn net.Conn) *imapClient {
	_ = conn.SetDeadline(time.Now().Add(sessionTimeout))

	return &imapClient{conn: conn, r: bufio.NewReaderSize(conn, 32*1024)}
}

// response is one server response: its text with every literal cut out and
// replaced by literalMark, and the literals themselves in order.
type response struct {
	text     string
	literals [][]byte
}

const literalMark = "\x00"

var literalRe = regexp.MustCompile(`\{(\d+)\}$`)

// maxLiteral refuses a literal larger than anything a poll asks for, so a
// hostile or broken server cannot make the control plane allocate at will.
const maxLiteral = 4 * 1024 * 1024

// readResponse reads one response, following every literal it announces.
func (c *imapClient) readResponse() (response, error) {
	var resp response
	var text strings.Builder

	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return resp, fmt.Errorf("reading from the mail server: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")

		m := literalRe.FindStringSubmatchIndex(line)
		if m == nil {
			text.WriteString(line)
			resp.text = text.String()

			return resp, nil
		}

		size, err := strconv.Atoi(line[m[2]:m[3]])
		if err != nil || size < 0 || size > maxLiteral {
			return resp, fmt.Errorf("the mail server sent a literal of %q bytes", line[m[2]:m[3]])
		}

		text.WriteString(line[:m[0]])
		text.WriteString(literalMark)

		lit := make([]byte, size)
		if _, err := io.ReadFull(c.r, lit); err != nil {
			return resp, fmt.Errorf("reading from the mail server: %w", err)
		}

		resp.literals = append(resp.literals, lit)
	}
}

func (c *imapClient) greeting() error {
	resp, err := c.readResponse()
	if err != nil {
		return err
	}

	if !strings.HasPrefix(resp.text, "* OK") && !strings.HasPrefix(resp.text, "* PREAUTH") {
		return fmt.Errorf("unexpected greeting from the mail server: %s", clip(resp.text))
	}

	return nil
}

// command sends one command and collects its untagged responses up to the
// tagged completion, which must be OK.
func (c *imapClient) command(format string, args ...any) ([]response, error) {
	c.tag++
	tag := "d" + strconv.Itoa(c.tag)

	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, fmt.Sprintf(format, args...)); err != nil {
		return nil, fmt.Errorf("writing to the mail server: %w", err)
	}

	var untagged []response

	for {
		resp, err := c.readResponse()
		if err != nil {
			return nil, err
		}

		if !strings.HasPrefix(resp.text, tag+" ") {
			untagged = append(untagged, resp)
			continue
		}

		status := strings.TrimPrefix(resp.text, tag+" ")
		if strings.HasPrefix(status, "OK") {
			return untagged, nil
		}

		return untagged, &statusError{text: status}
	}
}

// statusError is a NO or BAD completion, carrying the server's own words.
type statusError struct{ text string }

func (e *statusError) Error() string { return "the mail server said: " + clip(e.text) }

func (c *imapClient) login(user, password string) error {
	u, err := quoteIMAP(user)
	if err != nil {
		return err
	}

	p, err := quoteIMAP(password)
	if err != nil {
		return err
	}

	if _, err := c.command("LOGIN %s %s", u, p); err != nil {
		var se *statusError
		if errors.As(err, &se) {
			return fmt.Errorf("%w (%s)", ErrAuth, clip(se.text))
		}

		return err
	}

	return nil
}

var (
	uidValidityRe = regexp.MustCompile(`(?i)\[UIDVALIDITY (\d+)\]`)
	uidNextRe     = regexp.MustCompile(`(?i)\[UIDNEXT (\d+)\]`)
	uidItemRe     = regexp.MustCompile(`(?i)\bUID (\d+)`)
)

func (c *imapClient) Examine(folder string) (uint32, uint32, error) {
	f, err := quoteIMAP(folder)
	if err != nil {
		return 0, 0, err
	}

	untagged, err := c.command("EXAMINE %s", f)
	if err != nil {
		return 0, 0, fmt.Errorf("opening %q: %w", folder, err)
	}

	var validity, next uint32

	for _, resp := range untagged {
		if m := uidValidityRe.FindStringSubmatch(resp.text); m != nil {
			validity = parseUint32(m[1])
		}

		if m := uidNextRe.FindStringSubmatch(resp.text); m != nil {
			next = parseUint32(m[1])
		}
	}

	if validity == 0 {
		return 0, 0, fmt.Errorf("the mail server did not say the UIDVALIDITY of %q", folder)
	}

	// UIDNEXT is optional in IMAP4rev1; every server this is used with sends
	// it, but a missing one is recoverable from the highest UID there is.
	if next == 0 {
		uids, err := c.UIDsAfter(0)
		if err != nil {
			return 0, 0, err
		}

		next = 1
		if len(uids) > 0 {
			next = uids[len(uids)-1] + 1
		}
	}

	return validity, next, nil
}

func (c *imapClient) UIDsAfter(after uint32) ([]uint32, error) {
	untagged, err := c.command("UID SEARCH UID %d:*", after+1)
	if err != nil {
		return nil, err
	}

	var uids []uint32

	for _, resp := range untagged {
		if !strings.HasPrefix(strings.ToUpper(resp.text), "* SEARCH") {
			continue
		}

		for _, field := range strings.Fields(resp.text[len("* SEARCH"):]) {
			// "n:*" always includes the highest UID, even when it is below n.
			if uid := parseUint32(field); uid > after {
				uids = append(uids, uid)
			}
		}
	}

	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	return uids, nil
}

func (c *imapClient) Headers(uids []uint32) (map[uint32][]byte, error) {
	if len(uids) == 0 {
		return map[uint32][]byte{}, nil
	}

	set := make([]string, len(uids))
	for i, uid := range uids {
		set[i] = strconv.FormatUint(uint64(uid), 10)
	}

	untagged, err := c.command("UID FETCH %s (UID BODY.PEEK[HEADER])", strings.Join(set, ","))
	if err != nil {
		return nil, err
	}

	return fetched(untagged), nil
}

func (c *imapClient) Text(uid uint32) ([]byte, error) {
	untagged, err := c.command("UID FETCH %d (UID BODY.PEEK[TEXT]<0.%d>)", uid, bodyBytes)
	if err != nil {
		return nil, err
	}

	return fetched(untagged)[uid], nil
}

// fetched maps each FETCH response that carries a UID and a literal to that
// literal - the one section asked for. Unsolicited FETCH responses (a flag
// changing while the poll runs) carry no literal and are skipped.
func fetched(untagged []response) map[uint32][]byte {
	out := map[uint32][]byte{}

	for _, resp := range untagged {
		if len(resp.literals) == 0 || !strings.Contains(strings.ToUpper(resp.text), "FETCH") {
			continue
		}

		m := uidItemRe.FindStringSubmatch(strings.ReplaceAll(resp.text, literalMark, " "))
		if m == nil {
			continue
		}

		out[parseUint32(m[1])] = resp.literals[0]
	}

	return out
}

func (c *imapClient) Close() {
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.command("LOGOUT")
	c.conn.Close()
}

// quoteIMAP renders s as an IMAP quoted string. A value with a line break or
// a NUL cannot be quoted (it would need a literal, which LOGIN over a
// synchronising literal makes a round trip of its own); no real address,
// folder or app password contains one, so it is refused rather than supported.
func quoteIMAP(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", fmt.Errorf("a line break is not allowed here")
	}

	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)

	return `"` + s + `"`, nil
}

func parseUint32(s string) uint32 {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}

	return uint32(n)
}

// clip keeps a server's words short enough to show on a page.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "..."
	}

	return s
}
