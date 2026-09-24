package mailtrigger

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Message is what a rule is matched against: the headers of one email, read
// the way a person would read them (encoded words decoded, the address pulled
// out of the From line).
type Message struct {
	UID uint32
	// MessageID is the Message-ID header, kept on a job so the page can tell
	// two emails with the same subject apart.
	MessageID string
	// From is the whole From line ("Ada Lovelace <ada@example.com>"), and
	// Sender only its address, lowercased.
	From     string
	Sender   string
	Subject  string
	Received time.Time
	// Verified says the receiving server itself checked that the email really
	// comes from Sender's domain (DMARC, or DKIM signed by that domain), and
	// VerifyNote says what it found either way.
	Verified   bool
	VerifyNote string

	header []byte
}

// charsetReader lets the header decoder and the body reader handle the
// charsets that are one byte per character (and plain ASCII); anything else
// is left as the bytes it arrived as rather than failing the whole message.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252", "iso-8859-15":
		raw, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}

		return strings.NewReader(latin1(raw)), nil
	default:
		return nil, fmt.Errorf("unsupported charset %q", charset)
	}
}

func latin1(raw []byte) string {
	var b strings.Builder
	for _, c := range raw {
		b.WriteRune(rune(c))
	}

	return b.String()
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// decodeHeader decodes RFC 2047 encoded words, keeping the raw text when a
// charset is not supported.
func decodeHeader(value string) string {
	decoded, err := wordDecoder.DecodeHeader(value)
	if err != nil {
		return value
	}

	return decoded
}

// parseHeader reads one message's header block.
func parseHeader(uid uint32, header []byte, host string) (Message, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(ensureHeaderEnd(header)))
	if err != nil {
		return Message{}, fmt.Errorf("reading the headers of message %d: %w", uid, err)
	}

	m := Message{
		UID:       uid,
		MessageID: strings.TrimSpace(msg.Header.Get("Message-Id")),
		From:      strings.TrimSpace(decodeHeader(msg.Header.Get("From"))),
		Subject:   strings.TrimSpace(decodeHeader(msg.Header.Get("Subject"))),
		header:    header,
	}

	parser := mail.AddressParser{WordDecoder: wordDecoder}
	if addr, err := parser.Parse(msg.Header.Get("From")); err == nil {
		m.Sender = strings.ToLower(addr.Address)
	} else if found := addressRe.FindString(m.From); found != "" {
		m.Sender = strings.ToLower(found)
	}

	if when, err := msg.Header.Date(); err == nil {
		m.Received = when.UTC()
	}

	m.Verified, m.VerifyNote = verifySender(msg.Header, m.Sender, host)

	return m, nil
}

var addressRe = regexp.MustCompile(`[^\s<>"',;]+@[^\s<>"',;]+`)

// ensureHeaderEnd makes sure a header block ends with its blank line, which
// net/mail needs to know the headers are over.
func ensureHeaderEnd(header []byte) []byte {
	if bytes.HasSuffix(header, []byte("\r\n\r\n")) || bytes.HasSuffix(header, []byte("\n\n")) {
		return header
	}

	return append(append([]byte{}, bytes.TrimRight(header, "\r\n")...), "\r\n\r\n"...)
}

// trustedAuthServ is the name a provider puts at the start of the
// Authentication-Results header it adds itself. Only that one is believed: a
// sender can write any Authentication-Results header it likes into its own
// email, but the provider's is added on top, where a sender cannot put one.
func trustedAuthServ(host string) string {
	switch strings.ToLower(host) {
	case "imap.gmail.com":
		return "mx.google.com"
	case "outlook.office365.com", "imap-mail.outlook.com":
		return "" // Microsoft's authserv-id varies by tenant; the topmost header is taken as theirs.
	default:
		return ""
	}
}

var (
	dmarcPassRe = regexp.MustCompile(`(?i)\bdmarc=pass\b[^;]*?\bheader\.from=([^\s;]+)`)
	dkimPassRe  = regexp.MustCompile(`(?i)\bdkim=pass\b[^;]*?\bheader\.(?:i|d)=@?([^\s;]+)`)
)

// verifySender reads the receiving server's own verdict on whether the email
// comes from where its From line says. It is the provider's check, not one
// made here: this only reads the result the provider wrote at the top of the
// headers. A From line is trivial to forge, and a rule keyed on a sender is
// only as good as that check - so a rule can insist on it (Rule.VerifiedOnly).
func verifySender(h mail.Header, sender, host string) (bool, string) {
	results := h["Authentication-Results"]
	if len(results) == 0 {
		return false, "the email carries no Authentication-Results header from your provider"
	}

	top := strings.Join(strings.Fields(results[0]), " ")

	if want := trustedAuthServ(host); want != "" {
		authServ := strings.TrimSpace(strings.SplitN(top, ";", 2)[0])
		if !strings.EqualFold(authServ, want) {
			return false, fmt.Sprintf("the topmost Authentication-Results is from %q, not %s", clip(authServ), want)
		}
	}

	at := strings.LastIndex(sender, "@")
	if at < 0 {
		return false, "the From line has no address"
	}

	domain := sender[at+1:]

	for _, m := range dmarcPassRe.FindAllStringSubmatch(top, -1) {
		if sameDomain(m[1], domain) {
			return true, "DMARC passed for " + domain
		}
	}

	for _, m := range dkimPassRe.FindAllStringSubmatch(top, -1) {
		if sameDomain(m[1], domain) {
			return true, "DKIM passed for " + domain
		}
	}

	return false, "your provider did not confirm the email comes from " + domain
}

// sameDomain reports whether signed is domain or a parent of it.
func sameDomain(signed, domain string) bool {
	signed = strings.ToLower(strings.Trim(signed, "."))
	domain = strings.ToLower(domain)

	if at := strings.LastIndex(signed, "@"); at >= 0 {
		signed = signed[at+1:]
	}

	return signed == domain || strings.HasSuffix(domain, "."+signed)
}

// bodyText turns a message (its header plus the start of its body) into
// readable plain text, at most limit characters: the text/plain part when
// there is one, otherwise the HTML part with its markup taken out.
func bodyText(header, text []byte, limit int) string {
	msg, err := mail.ReadMessage(io.MultiReader(bytes.NewReader(ensureHeaderEnd(header)), bytes.NewReader(text)))
	if err != nil {
		return ""
	}

	plain, htmlText := walkPart(msg.Header, msg.Body, 0)

	out := plain
	if strings.TrimSpace(out) == "" {
		out = stripHTML(htmlText)
	}

	return truncate(tidy(out), limit)
}

// partHeader is a message's or a MIME part's header.
type partHeader interface {
	Get(key string) string
}

// walkPart collects the first text/plain and text/html content under a part,
// descending into multipart containers (a mixed message holding an
// alternative one is the ordinary shape). A body cut short by the fetch limit
// ends the walk where it breaks, keeping whatever was read.
func walkPart(h partHeader, body io.Reader, depth int) (plain, htmlText string) {
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}

	if strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" && depth < 5 {
		reader := multipart.NewReader(body, params["boundary"])

		for {
			part, err := reader.NextRawPart()
			if err != nil {
				return plain, htmlText
			}

			if strings.HasPrefix(strings.ToLower(part.Header.Get("Content-Disposition")), "attachment") {
				continue
			}

			p, hText := walkPart(part.Header, part, depth+1)
			if plain == "" {
				plain = p
			}

			if htmlText == "" {
				htmlText = hText
			}
		}
	}

	if mediaType != "text/plain" && mediaType != "text/html" {
		return "", ""
	}

	decoded := decodeTransfer(h.Get("Content-Transfer-Encoding"), body)

	text := decoded
	if r, err := charsetReader(params["charset"], strings.NewReader(decoded)); err == nil {
		if raw, err := io.ReadAll(r); err == nil {
			text = string(raw)
		}
	}

	text = strings.ToValidUTF8(text, "�")

	if mediaType == "text/html" {
		return "", text
	}

	return text, ""
}

// decodeTransfer undoes a part's Content-Transfer-Encoding, keeping what
// decoded before any error (a truncated body always ends in one).
func decodeTransfer(encoding string, body io.Reader) string {
	var r io.Reader

	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	default:
		r = body
	}

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, io.LimitReader(r, bodyBytes*2))

	return buf.String()
}

var (
	scriptRe     = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)>`)
	blockRe      = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/tr|/h[1-6]|/li)\b[^>]*>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRunRe   = regexp.MustCompile(`[ \t\x{00A0}]+`)
	blankLinesRe = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string {
	s = scriptRe.ReplaceAllString(s, "")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")

	return html.UnescapeString(s)
}

func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = spaceRunRe.ReplaceAllString(s, " ")

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}

	return strings.TrimSpace(blankLinesRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// truncate cuts s to at most limit characters, on a character boundary.
func truncate(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}

	runes := []rune(s)

	return string(runes[:limit]) + "\n[... the rest of the email was cut]"
}
