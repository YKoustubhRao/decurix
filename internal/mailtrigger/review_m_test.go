package mailtrigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/smtp"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Regression tests for the round-1 review of mail triggers and sending
// (B-15, B-16, B-17, B-28).

// mustRender is RenderWithFiles for a request that must render.
func mustRender(rule Rule, msg Message, text, dir string, files []Attachment) string {
	out, err := RenderWithFiles(rule, msg, text, dir, files, nil)
	if err != nil {
		panic(err)
	}
	return out
}

func TestNamedDomainReadsThePatternAsPlainText(t *testing.T) {
	for pattern, want := range map[string]string{
		`billing@acme.com`:       "acme.com",
		`^billing@acme\.com$`:    "acme.com",
		`@acme\.com$`:            "acme.com",
		`acme.co.in`:             "acme.co.in",
		`.*@Acme.COM`:            "acme.com",
		`acme`:                   "",
		`@(acme|other)\.com`:     "",
		`Ada Lovelace`:           "",
		`billing@acme.com.*`:     "",
		`billing@acme\.com|evil`: "",
	} {
		if got := NamedDomain(pattern); got != want {
			t.Errorf("NamedDomain(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// A verified-only rule names a domain; a verified email from a different
// domain that merely contains the pattern's text is skipped, while a verified
// email from that domain or a subdomain of it is sent.
func TestAVerifiedLookAlikeDomainIsSkipped(t *testing.T) {
	rule := Rule{ID: "r", Enabled: true, SenderRe: "billing@acme.com", VerifiedOnly: true, Content: "File it"}
	m := compileRules([]Rule{rule})[0]

	for sender, want := range map[string]string{
		"billing@acme.com.example.net": JobSkipped,
		"billing@acme.com":             JobPending,
		"billing@mail.acme.com":        JobPending,
	} {
		msg := Message{From: sender, Sender: sender, Subject: "Invoice", Verified: true, VerifyNote: "DKIM passed for " + sender[strings.Index(sender, "@")+1:]}
		if sender != "billing@mail.acme.com" && !m.matches(msg) {
			t.Fatalf("the pattern should match %s as text; the domain check is what refuses it", sender)
		}
		job := newJob(Account{ID: "a"}, rule, msg, "", time.Now(), nil, nil)
		if job.Status != want {
			t.Errorf("%s: status %s (%s), want %s", sender, job.Status, job.Note, want)
		}
	}
}

func TestAVerifiedOnlyRuleMustNameItsDomain(t *testing.T) {
	h := newHarness(t)
	_, err := h.store.CreateRule(NewRule{AccountID: h.acct.ID, DesktopID: "d", SenderRe: "acme", Content: "x", VerifiedOnly: true})
	if err == nil {
		t.Fatal("a verified-only rule whose sender pattern names no domain was saved")
	}
	if _, err := h.store.CreateRule(NewRule{AccountID: h.acct.ID, DesktopID: "d", SenderRe: `@acme\.com$`, Content: "x", VerifiedOnly: true}); err != nil {
		t.Fatal(err)
	}

	// A rule saved before the check (a pattern with no domain) skips rather than sends.
	old := Rule{ID: "r", SenderRe: "acme", VerifiedOnly: true, Content: "x"}
	job := newJob(Account{}, old, Message{Sender: "x@acme.com", Verified: true}, "", time.Now(), nil, nil)
	if job.Status != JobSkipped {
		t.Fatalf("status %s", job.Status)
	}
}

// optionRE is how the agent reads a command's options (replay_task.OPTION_RE).
var optionRE = regexp.MustCompile(`([A-Za-z_]\w*)=(?:"([^"]*)"|'([^']*)'|(\S+))`)

func optionsOf(line string) map[string]string {
	params := map[string]string{}
	for _, m := range optionRE.FindAllStringSubmatch(line, -1) {
		for _, v := range m[2:] {
			if v != "" {
				params[m[1]] = v
				break
			}
		}
	}
	return params
}

// An email's subject fills one option of a command and nothing more.
func TestAnEmailValueIsOneTokenInACommand(t *testing.T) {
	plain := Message{Subject: "Order 7"}
	for content, want := range map[string]string{
		`/skill ship-order order="{subject}"`: `/skill ship-order order="Order 7"`,
		`/skill ship-order order={subject}`:   `/skill ship-order order="Order 7"`,
	} {
		first, _, _ := strings.Cut(mustRender(Rule{Content: content}, plain, "", "", nil), "\n")
		if first != want {
			t.Errorf("%s -> %q, want %q", content, first, want)
		}
	}

	for _, subject := range []string{`Order 7" warehouse="Other`, `7 warehouse=Other`} {
		for _, content := range []string{`/skill ship-order order="{subject}"`, `/skill ship-order order={subject}`} {
			out, err := RenderWithFiles(Rule{Content: content}, Message{Subject: subject}, "", "", nil, nil)
			if err == nil {
				first, _, _ := strings.Cut(out, "\n")
				if _, ok := optionsOf(first)["warehouse"]; ok {
					t.Errorf("subject %q set an option the rule never named: %q", subject, first)
				}
			}
		}
	}

	// A space outside an option's quotes would be a second word.
	if _, err := RenderWithFiles(Rule{Content: `/replay {subject}`}, plain, "", "", nil, nil); err == nil {
		t.Error("a two-word value went into a command as two words")
	}
	if out, err := RenderWithFiles(Rule{Content: `/replay {subject}`}, Message{Subject: "demo-1"}, "", "", nil, nil); err != nil || out != "/replay demo-1" {
		t.Errorf("one word: %q, %v", out, err)
	}
}

// Whether a request is a command is the rule's choice, not the email's.
func TestAnEmailCannotMakeARequestACommand(t *testing.T) {
	got := mustRender(Rule{Content: "{subject}", IncludeBody: true}, Message{Subject: "/replay demo-1"}, "body", "", nil)
	if strings.HasPrefix(strings.TrimSpace(got), "/") {
		t.Fatalf("the email's subject made the request a command: %q", got)
	}
	if !strings.Contains(got, emailStart) {
		t.Fatalf("the request lost its email frame: %q", got)
	}
}

// A job whose command cannot be filled is skipped with nothing to send.
func TestAnUnfillableCommandIsSkippedNotSent(t *testing.T) {
	rule := Rule{ID: "r", Content: `/skill ship-order order="{subject}"`}
	job := newJob(Account{}, rule, Message{Subject: `a"b`}, "", time.Now(), nil, nil)
	if job.Status != JobSkipped || job.Content != "" || !strings.Contains(job.Note, "subject") {
		t.Fatalf("job = %+v", job)
	}
}

// A recipient or a file type with a line break never reaches the header block.
func TestAHeaderValueWithALineBreakIsRefused(t *testing.T) {
	for _, to := range []string{"Name (c\r\nX-Extra: 1) <a@x.com>", "a@x.com\r\nBcc: z@y.com"} {
		if _, err := BuildMessage("me@x.com", OutgoingMail{To: []string{to}, Subject: "s", Body: "b"}, time.Unix(0, 0)); err == nil {
			t.Errorf("an address with a line break was accepted: %q", to)
		}
	}

	raw, err := BuildMessage("me@x.com", OutgoingMail{To: []string{`"Ada, L." <a@x.com>`}, Subject: "s", Body: "b",
		Attachments: []Attachment{{Name: "f\r\n.pdf", ContentType: "application/pdf\r\nX-Extra: yes", Data: []byte("x")}}}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\r\nX-Extra") {
		t.Fatalf("a content type with a line break added a header:\n%s", raw)
	}
	if !strings.Contains(string(raw), `To: "Ada, L." <a@x.com>`) {
		t.Fatalf("the recipient was not rendered from its parsed form:\n%s", raw)
	}
	if !strings.Contains(string(raw), "Content-Type: application/octet-stream; name=f.pdf") {
		t.Fatalf("an unparsable content type was not replaced:\n%s", raw)
	}
}

type fakeConversation struct {
	quitErr error
	dataErr error
	closed  bool
}

func (f *fakeConversation) Auth(smtp.Auth) error { return nil }
func (f *fakeConversation) Mail(string) error    { return nil }
func (f *fakeConversation) Rcpt(string) error    { return nil }
func (f *fakeConversation) Quit() error          { return f.quitErr }
func (f *fakeConversation) Data() (io.WriteCloser, error) {
	return closer{f}, nil
}

type closer struct{ f *fakeConversation }

func (c closer) Write(p []byte) (int, error) { return len(p), nil }
func (c closer) Close() error                { c.f.closed = true; return c.f.dataErr }

// Once the server accepted the email, a failed goodbye does not make it unsent.
func TestAFailedQuitAfterTheServerAcceptedIsSent(t *testing.T) {
	c := &fakeConversation{quitErr: errors.New("connection reset")}
	if err := converse(c, nil, "me@x.com", []string{"a@x.com"}, []byte("m")); err != nil {
		t.Fatalf("an accepted email was reported unsent: %v", err)
	}

	c = &fakeConversation{dataErr: errors.New("552 too big")}
	if err := converse(c, nil, "me@x.com", []string{"a@x.com"}, []byte("m")); err == nil {
		t.Fatal("an email the server refused was reported sent")
	}
}

// Files an email carried that could not be kept are named in the request.
func TestDroppedAttachmentsAreNamedInTheRequest(t *testing.T) {
	var parts strings.Builder
	parts.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n")
	for i := 0; i < maxAttachments+2; i++ {
		fmt.Fprintf(&parts, "--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=f%02d.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nQUJD\r\n", i)
	}
	// A part whose encoding is broken.
	parts.WriteString("--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=broken.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n@@@@\r\n--b--\r\n")
	raw := "From: a@x.com\r\nSubject: s\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" + parts.String()

	files, dropped := ExtractAttachments([]byte(raw))
	if len(files) != maxAttachments {
		t.Fatalf("kept %d files", len(files))
	}

	out := mustRenderDropped(Rule{Content: "Enter the orders"}, files, dropped)
	for _, name := range []string{"f20.pdf", "f21.pdf"} {
		if !strings.Contains(out, name) {
			t.Errorf("the request does not name %s, left out for the file limit:\n%s", name, out)
		}
	}
	if strings.Contains(out, "broken.pdf") == false {
		t.Errorf("the request does not name the file that could not be decoded:\n%s", out)
	}
	if !strings.Contains(out, "NOT on this desktop") {
		t.Errorf("the request does not say the files are missing:\n%s", out)
	}
}

func mustRenderDropped(rule Rule, files []Attachment, dropped []string) string {
	out, err := RenderWithFiles(rule, Message{From: "a@x.com", Subject: "s"}, "", "/d", files, dropped)
	if err != nil {
		panic(err)
	}
	return out
}

type fullSession struct {
	fakeSession
	raw map[uint32]string
}

func (s fullSession) Full(uid uint32) ([]byte, error) { return []byte(s.raw[uid]), nil }

func (h *harness) fileRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM mail_job_files`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The files of jobs that are gone go with them: a deleted rule's waiting job
// and a job pruned from the history leave no file rows behind.
func TestJobFilesGoWithTheirJobs(t *testing.T) {
	h := newHarness(t)
	rule := h.rule(t, NewRule{SubjectRe: `.`})
	h.sub.busy["bordu"] = "a task is running"

	raws := map[uint32]string{}
	h.runner.Dial = func(ctx context.Context, acct Account) (Mailbox, error) {
		mb, err := h.box.dialer(ctx, acct)
		if err != nil {
			return nil, err
		}
		return fullSession{mb.(fakeSession), raws}, nil
	}

	body := "--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=x.pdf\r\n\r\nPDF\r\n--b--\r\n"
	uid := h.box.deliver("x@y.example", "Order", body, "Content-Type: multipart/mixed; boundary=b\r\n")
	raws[uid] = h.box.headers[uid] + body

	h.advance(2 * time.Minute)
	h.runner.step(context.Background())
	if n := h.fileRows(t); n != 1 {
		t.Fatalf("file rows = %d", n)
	}

	if err := h.store.DeleteRule(rule.ID); err != nil {
		t.Fatal(err)
	}
	if n := h.fileRows(t); n != 0 {
		t.Fatalf("%d file rows remain after their job was deleted", n)
	}

	// Pruning: a finished job beyond the history with files left over.
	if _, err := h.db.Exec(`INSERT INTO mail_job_files (job_id, position, name, content_type, data) VALUES ('gone', 0, 'a', 'b', x'00')`); err != nil {
		t.Fatal(err)
	}
	h.store.mu.Lock()
	h.store.pruneLocked()
	h.store.mu.Unlock()
	if n := h.fileRows(t); n != 0 {
		t.Fatalf("prune left %d orphaned file rows", n)
	}
}

type stuckSession struct {
	fakeSession
	bad uint32
}

func (s stuckSession) Text(uid uint32) ([]byte, error) {
	if uid == s.bad {
		return nil, errors.New("the mail server said: NO message unavailable")
	}
	return s.fakeSession.Text(uid)
}

// An email the server never returns is retried, then passed over with a
// failed job a person can see, and the emails after it go on.
func TestAnUnreadableEmailIsPassedOverAfterThreeTries(t *testing.T) {
	h := newHarness(t)
	h.rule(t, NewRule{SubjectRe: `.`, IncludeBody: true})
	bad := h.box.deliver("a@x.com", "Broken", "x")
	h.box.deliver("a@x.com", "Next one", "y")
	h.runner.Dial = func(ctx context.Context, acct Account) (Mailbox, error) {
		mb, err := h.box.dialer(ctx, acct)
		if err != nil {
			return nil, err
		}
		return stuckSession{mb.(fakeSession), bad}, nil
	}

	for i := 0; i < maxReadTries-1; i++ {
		h.advance(20 * time.Minute)
		h.runner.step(context.Background())
		if n := len(h.jobs(t)); n != 0 {
			t.Fatalf("after %d failed reads, %d jobs (the email should be retried, whole)", i+1, n)
		}
	}

	h.advance(20 * time.Minute)
	h.runner.step(context.Background())

	jobs := h.jobs(t)
	var failed, sent int
	for _, j := range jobs {
		switch {
		case j.Subject == "Broken" && j.Status == JobFailed && strings.Contains(j.Note, "would not return"):
			failed++
		case j.Subject == "Next one" && j.Status == JobSent:
			sent++
		}
	}
	if failed != 1 || sent != 1 {
		t.Fatalf("want the unreadable email failed and the next one sent; jobs = %+v", jobs)
	}

	acct, _ := h.store.Account(h.acct.ID)
	if acct.stuckUID != 0 || acct.stuckTries != 0 {
		t.Fatalf("the mailbox is still marked stuck: %d, %d", acct.stuckUID, acct.stuckTries)
	}
}

// panicSession panics while reading one email's headers batch.
type panicSession struct {
	fakeSession
	on uint32
}

func (s panicSession) Headers(uids []uint32) (map[uint32][]byte, error) {
	for _, u := range uids {
		if u == s.on {
			panic("bad header block")
		}
	}
	return s.fakeSession.Headers(uids)
}

// Batches already read are committed: a later batch that fails does not lose
// or repeat them.
func TestEachBatchIsCommittedAsItIsRead(t *testing.T) {
	h := newHarness(t)
	h.rule(t, NewRule{SubjectRe: `.`})
	h.sub.stickBusy = true

	var last uint32
	for i := 0; i < batchSize+1; i++ {
		last = h.box.deliver("a@x.com", fmt.Sprintf("Mail %d", i), "")
	}
	h.runner.Dial = func(ctx context.Context, acct Account) (Mailbox, error) {
		mb, err := h.box.dialer(ctx, acct)
		if err != nil {
			return nil, err
		}
		return panicSession{mb.(fakeSession), last}, nil
	}

	h.advance(2 * time.Minute)
	acct, _ := h.store.Account(h.acct.ID)
	_ = h.runner.poll(context.Background(), acct)

	if n := len(h.jobs(t)); n != batchSize {
		t.Fatalf("queued %d jobs; the first batch (%d) should be saved", n, batchSize)
	}
	acct, _ = h.store.Account(h.acct.ID)
	if acct.lastUID != last-1 {
		t.Fatalf("position %d, want %d", acct.lastUID, last-1)
	}
}

// A job that has no request cannot be run again: there is nothing to send.
func TestAJobWithNothingToSendCannotBeRetried(t *testing.T) {
	h := newHarness(t)
	rule := h.rule(t, NewRule{SubjectRe: `.`, Content: `/skill ship-order order="{subject}"`})
	h.box.deliver("a@x.com", `a"b`, "")
	h.advance(2 * time.Minute)
	h.runner.step(context.Background())

	jobs := h.jobs(t)
	if len(jobs) != 1 || jobs[0].Status != JobSkipped || jobs[0].RuleID != rule.ID {
		t.Fatalf("jobs = %+v", jobs)
	}
	if err := h.store.RetryJob(jobs[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry of an empty request: %v", err)
	}
}

func TestUniqueNamesNeverRepeatAName(t *testing.T) {
	got := uniqueNames([]Attachment{{Name: "a.pdf"}, {Name: "a.pdf"}, {Name: "a (2).pdf"}, {Name: "A.PDF"}})
	seen := map[string]bool{}
	for _, f := range got {
		if seen[strings.ToLower(f.Name)] {
			t.Fatalf("duplicate %q in %v", f.Name, got)
		}
		seen[strings.ToLower(f.Name)] = true
	}
	if got[0].Name != "a.pdf" || got[2].Name != "a (2).pdf" {
		t.Fatalf("a name that was already unique changed: %v", got)
	}
}
