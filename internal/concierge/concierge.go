package concierge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// Deps is everything the concierge needs from the rest of the control plane,
// as plain functions so it depends on no HTTP code and tests need no Docker.
// cmd/decurix wires them to *httpapi.Server and its stores.
type Deps struct {
	// Desktops lists every desktop (desktop.Manager.List).
	Desktops func(ctx context.Context) ([]desktop.Desktop, error)
	// Busy says why a desktop cannot take a new task ("" when free) -
	// tasks.Store.Busy.
	Busy func(desktopID string) (string, error)
	// Send delivers a person's words to a desktop through the same path the
	// chat page uses (Plan, deliver, Commit), and returns the task ID and the
	// route kind (new/reply/retry) it was recorded under.
	Send func(ctx context.Context, desktopID, content string) (taskID, routeKind string, err error)
	// Transcript is the desktop's chat snapshot (conversation.Store.Snapshot),
	// where a session's replies are read from.
	Transcript func(desktopID string) conversation.Thread
	// TaskStatus is a task record's status (tasks.Store.Get); optional. It
	// is how a session learns its desktop was stopped under it.
	TaskStatus func(taskID string) (string, error)
	// Suggest names the desktop best suited to a request; optional.
	Suggest func(ctx context.Context, content string) (desktopID string, ok bool)
	// Stop stops a desktop (the dashboard's Stop); optional.
	Stop func(ctx context.Context, desktopID string) error
}

// Concierge routes a person's messages to worker sessions and relays what
// those sessions say back into one conversation.
type Concierge struct {
	Store *Store
	Deps  Deps
	// Interval is how often active sessions are checked (default 1.5s).
	Interval time.Duration

	// queue serialises processing, so two quick messages are handled in the
	// order they were typed and never pick the same free desktop.
	queue   chan string
	running bool
	mu      sync.Mutex
	// work serialises Process and Tick: both read and write sessions.
	work sync.Mutex
}

// New returns a Concierge over store and deps.
func New(store *Store, deps Deps) *Concierge {
	return &Concierge{Store: store, Deps: deps, Interval: 1500 * time.Millisecond, queue: make(chan string, 64)}
}

// Handle records what a person typed and queues it for processing, returning
// at once: starting a stopped desktop can take a minute, and the page must
// not wait on it. Without Run it processes in line (tests, a CLI).
func (c *Concierge) Handle(ctx context.Context, text string) (Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, errors.New("empty message")
	}
	msg, err := c.Store.AddMessage(RoleUser, 0, "", text)
	if err != nil {
		return Message{}, err
	}

	c.mu.Lock()
	running := c.running
	c.mu.Unlock()

	if !running {
		c.Process(ctx, text)
		return msg, nil
	}
	select {
	case c.queue <- text:
	default:
		c.say(0, "", "I'm still working through your last few messages - send that again in a moment.")
	}
	return msg, nil
}

// Run processes queued messages and watches active sessions until ctx ends.
func (c *Concierge) Run(ctx context.Context) {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	interval := c.Interval
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case text := <-c.queue:
			c.Process(ctx, text)
		case <-ticker.C:
			c.Tick(ctx)
		}
	}
}

var (
	sessionRef = regexp.MustCompile(`^#(\d+)\b\s*(.*)$`)
	desktopRef = regexp.MustCompile(`^@([A-Za-z0-9._-]+)\s*(.*)$`)
	stopCmd    = regexp.MustCompile(`(?i)^stop\s+(#\d+|@[A-Za-z0-9._-]+)\s*$`)
	// A short yes/no is an answer, not a task - it goes to the one session
	// waiting on a person, and is never started as a new task.
	shortAnswer = regexp.MustCompile(`(?i)^(y|yes|yep|yeah|ok|okay|sure|approve|approved|go|go ahead|do it|proceed|confirm|confirmed|send it|lgtm|n|no|nope|deny|denied|reject|rejected|don'?t|do not|cancel)[.! ]*$`)
)

// Process handles one message a person typed. Exported for tests and for a
// caller that wants to wait for the outcome.
func (c *Concierge) Process(ctx context.Context, text string) {
	c.work.Lock()
	defer c.work.Unlock()

	text = strings.TrimSpace(text)
	lower := strings.ToLower(text)

	switch lower {
	case "help", "?", "/help":
		c.say(0, "", helpText)
		return
	case "status", "list", "ls", "desktops", "/status", "what's running", "whats running":
		c.say(0, "", c.statusText(ctx))
		return
	case "sessions", "history", "/sessions":
		c.say(0, "", c.sessionsText())
		return
	}

	if m := stopCmd.FindStringSubmatch(text); m != nil {
		c.stop(ctx, m[1])
		return
	}

	if m := sessionRef.FindStringSubmatch(text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		c.reply(ctx, n, strings.TrimSpace(m[2]))
		return
	}

	if m := desktopRef.FindStringSubmatch(text); m != nil {
		c.onDesktop(ctx, m[1], strings.TrimSpace(m[2]))
		return
	}

	if shortAnswer.MatchString(text) {
		c.answerWaiting(ctx, text)
		return
	}

	c.newTask(ctx, "", text)
}

const helpText = `Tell me what you want done and I'll start it on a free desktop. Each job gets a number (#1, #2, ...) and I'll report back here when it finishes or needs you.

  <anything>          start a new job on the best free desktop
  @name <request>     start it on that desktop
  #3 <reply>          answer job #3 (a question or an approval)
  yes / no            answer the one job waiting on you
  status              what every desktop is doing
  sessions            recent jobs
  stop #3 | stop @name  stop that desktop (ends what it is doing)`

// say appends a concierge message.
func (c *Concierge) say(sessionID int64, kind, text string) {
	if _, err := c.Store.AddMessage(RoleConcierge, sessionID, kind, text); err != nil {
		log.Printf("concierge: %v", err)
	}
}

// reply sends text to session n's desktop as the answer to what it asked.
func (c *Concierge) reply(ctx context.Context, n int64, text string) {
	sess, err := c.Store.Session(n)
	if errors.Is(err, ErrNoSession) {
		c.say(0, "", fmt.Sprintf("There is no job #%d. Type \"sessions\" to see them.", n))
		return
	}
	if err != nil {
		c.fail(err)
		return
	}
	if text == "" {
		c.say(sess.ID, "", fmt.Sprintf("What should I tell #%d? Type \"#%d\" followed by your reply.", n, n))
		return
	}

	taskID, route, err := c.Deps.Send(ctx, sess.DesktopID, text)
	if err != nil {
		c.say(sess.ID, "error", fmt.Sprintf("Could not reach %s: %v", sess.DesktopName, err))
		return
	}

	if taskID != sess.TaskID {
		// The desktop was no longer waiting on this job, so it took the
		// words as a new request - say so rather than pretend it answered.
		ns, err := c.Store.CreateSession(sess.DesktopID, sess.DesktopName, taskID, text)
		if err != nil {
			c.fail(err)
			return
		}
		c.say(ns.ID, "", fmt.Sprintf("#%d wasn't waiting any more, so %s took that as a new job: #%d.", sess.ID, sess.DesktopName, ns.ID))
		return
	}

	if err := c.Store.UpdateSession(sess.ID, StatusRunning, sess.Kind, sess.LastEntryID); err != nil {
		c.fail(err)
		return
	}
	verb := "Sent your reply to"
	if route == "retry" {
		verb = "Trying again:"
	}
	c.say(sess.ID, "", fmt.Sprintf("%s #%d on %s.", verb, sess.ID, sess.DesktopName))
}

// answerWaiting sends a short yes/no to the one session waiting on a person.
func (c *Concierge) answerWaiting(ctx context.Context, text string) {
	waiting, err := c.waiting()
	if err != nil {
		c.fail(err)
		return
	}
	switch len(waiting) {
	case 0:
		c.say(0, "", "Nothing is waiting on you right now.")
	case 1:
		c.reply(ctx, waiting[0].ID, text)
	default:
		var b strings.Builder
		b.WriteString("More than one job is waiting on you - which one? Reply with its number, e.g. \"#")
		b.WriteString(strconv.FormatInt(waiting[0].ID, 10))
		b.WriteString(" " + text + "\".")
		for _, s := range waiting {
			fmt.Fprintf(&b, "\n  #%d on %s: %s", s.ID, s.DesktopName, clip(s.Request, 70))
		}
		c.say(0, "", b.String())
	}
}

func (c *Concierge) waiting() ([]Session, error) {
	active, err := c.Store.ActiveSessions()
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, s := range active {
		if s.Status == StatusNeedsYou {
			out = append(out, s)
		}
	}
	return out, nil
}

// onDesktop handles "@name text": a new job there, or - if that desktop is
// waiting on a person - the answer to what it asked, since that is how the
// desktop itself will take it (tasks.Plan).
func (c *Concierge) onDesktop(ctx context.Context, name, text string) {
	d, ok, err := c.findDesktop(ctx, name)
	if err != nil {
		c.fail(err)
		return
	}
	if !ok {
		c.say(0, "", fmt.Sprintf("I don't know a desktop called %q. Type \"status\" to see them.", name))
		return
	}
	if text == "" {
		c.say(0, "", fmt.Sprintf("What should %s do? Type \"@%s\" followed by the request.", d.Name, d.ID))
		return
	}

	active, err := c.Store.ActiveSessions()
	if err != nil {
		c.fail(err)
		return
	}
	for _, s := range active {
		if s.DesktopID == d.ID && s.Status == StatusNeedsYou {
			c.reply(ctx, s.ID, text)
			return
		}
	}

	c.newTask(ctx, d.ID, text)
}

// newTask starts text as a new job, on desktopID when given, otherwise on
// the best free desktop.
func (c *Concierge) newTask(ctx context.Context, desktopID, text string) {
	desktops, err := c.Deps.Desktops(ctx)
	if err != nil {
		c.fail(err)
		return
	}

	var target desktop.Desktop
	if desktopID != "" {
		for _, d := range desktops {
			if d.ID == desktopID {
				target = d
			}
		}
		why, err := c.Deps.Busy(target.ID)
		if err != nil {
			c.fail(err)
			return
		}
		if why != "" {
			c.say(0, "", c.busyText(target, why))
			return
		}
		if !usable(target) {
			c.say(0, "", fmt.Sprintf("%s can't take work right now (%s).", target.Name, stateOf(target)))
			return
		}
	} else {
		var ok bool
		target, ok = c.pick(ctx, desktops, text)
		if !ok {
			c.say(0, "", c.noFreeDesktopText(desktops))
			return
		}
	}

	if target.Status != "running" {
		c.say(0, "", fmt.Sprintf("Waking %s up - this can take a minute...", target.Name))
	}

	taskID, _, err := c.Deps.Send(ctx, target.ID, text)
	if err != nil {
		c.say(0, "error", fmt.Sprintf("Could not start that on %s: %v", target.Name, err))
		return
	}
	sess, err := c.Store.CreateSession(target.ID, target.Name, taskID, text)
	if err != nil {
		c.fail(err)
		return
	}
	c.say(sess.ID, "started", fmt.Sprintf("#%d started on %s. I'll tell you when it's done or needs you.", sess.ID, target.Name))
}

// pick chooses the desktop for a new job: the suggested one when it is free,
// otherwise the first free running desktop, then a paused one, then a
// stopped one (sending wakes it). A crashed or provisioning desktop is never
// picked.
func (c *Concierge) pick(ctx context.Context, desktops []desktop.Desktop, text string) (desktop.Desktop, bool) {
	free := func(d desktop.Desktop) bool {
		if !usable(d) {
			return false
		}
		why, err := c.Deps.Busy(d.ID)
		return err == nil && why == ""
	}

	if c.Deps.Suggest != nil {
		if id, ok := c.Deps.Suggest(ctx, text); ok {
			for _, d := range desktops {
				if d.ID == id && free(d) {
					return d, true
				}
			}
		}
	}
	for _, want := range []string{"running", "paused", "stopped"} {
		for _, d := range desktops {
			if d.Status == want && free(d) {
				return d, true
			}
		}
	}
	return desktop.Desktop{}, false
}

// usable reports whether a desktop can be sent work at all.
func usable(d desktop.Desktop) bool {
	if d.ID == "" || d.Crash != "" {
		return false
	}
	switch d.Status {
	case "running", "paused", "stopped":
		return true
	}
	return false
}

func stateOf(d desktop.Desktop) string {
	if d.Crash != "" {
		return "crashed: " + d.Crash
	}
	if d.Status == "" {
		return "unknown"
	}
	return d.Status
}

// findDesktop resolves "@name" against desktop IDs and names, ignoring case.
func (c *Concierge) findDesktop(ctx context.Context, name string) (desktop.Desktop, bool, error) {
	desktops, err := c.Deps.Desktops(ctx)
	if err != nil {
		return desktop.Desktop{}, false, err
	}
	for _, d := range desktops {
		if strings.EqualFold(d.ID, name) || strings.EqualFold(d.Name, name) {
			return d, true, nil
		}
	}
	return desktop.Desktop{}, false, nil
}

// stop stops the desktop named by "#N" (that job's) or "@name".
func (c *Concierge) stop(ctx context.Context, ref string) {
	if c.Deps.Stop == nil {
		c.say(0, "", "Stopping isn't available here - use the Desktops page.")
		return
	}

	var d desktop.Desktop
	if strings.HasPrefix(ref, "#") {
		n, _ := strconv.ParseInt(ref[1:], 10, 64)
		sess, err := c.Store.Session(n)
		if err != nil {
			c.say(0, "", fmt.Sprintf("There is no job %s.", ref))
			return
		}
		d = desktop.Desktop{ID: sess.DesktopID, Name: sess.DesktopName}
	} else {
		found, ok, err := c.findDesktop(ctx, ref[1:])
		if err != nil {
			c.fail(err)
			return
		}
		if !ok {
			c.say(0, "", fmt.Sprintf("I don't know a desktop called %q.", ref[1:]))
			return
		}
		d = found
	}

	if err := c.Deps.Stop(ctx, d.ID); err != nil {
		c.say(0, "error", fmt.Sprintf("Could not stop %s: %v", d.Name, err))
		return
	}

	active, err := c.Store.ActiveSessions()
	if err != nil {
		c.fail(err)
		return
	}
	for _, s := range active {
		if s.DesktopID == d.ID {
			_ = c.Store.UpdateSession(s.ID, StatusStopped, s.Kind, s.LastEntryID)
		}
	}
	c.say(0, "", fmt.Sprintf("Stopped %s.", d.Name))
}

// Tick relays anything new from active sessions: a reply, a question, a
// failure, or that the desktop was stopped before it finished.
func (c *Concierge) Tick(ctx context.Context) {
	c.work.Lock()
	defer c.work.Unlock()

	active, err := c.Store.ActiveSessions()
	if err != nil {
		log.Printf("concierge: %v", err)
		return
	}

	threads := map[string]conversation.Thread{}
	for _, s := range active {
		th, ok := threads[s.DesktopID]
		if !ok {
			th = c.Deps.Transcript(s.DesktopID)
			threads[s.DesktopID] = th
		}

		if e, ok := latestReply(th, s.TaskID); ok && e.ID != s.LastEntryID {
			status := StatusDone
			switch e.Kind {
			case conversation.KindQuery, conversation.KindApproval:
				status = StatusNeedsYou
			case conversation.KindError:
				status = StatusFailed
			}
			if err := c.Store.UpdateSession(s.ID, status, e.Kind, e.ID); err != nil {
				log.Printf("concierge: %v", err)
				continue
			}
			if _, err := c.Store.AddMessage(RoleAgent, s.ID, e.Kind, e.Content); err != nil {
				log.Printf("concierge: %v", err)
			}
			continue
		}

		if s.Status == StatusRunning && c.Deps.TaskStatus != nil {
			st, err := c.Deps.TaskStatus(s.TaskID)
			if err == nil && st == "abandoned" {
				_ = c.Store.UpdateSession(s.ID, StatusStopped, s.Kind, s.LastEntryID)
				c.say(s.ID, "error", fmt.Sprintf("#%d ended without an answer: %s was stopped or restarted.", s.ID, s.DesktopName))
			}
		}
	}
}

// latestReply is the newest agent entry for taskID in a transcript.
func latestReply(th conversation.Thread, taskID string) (conversation.Entry, bool) {
	for i := len(th.Entries) - 1; i >= 0; i-- {
		e := th.Entries[i]
		if e.Role == conversation.RoleAgent && e.TaskID == taskID {
			return e, true
		}
	}
	return conversation.Entry{}, false
}

// liveStep is what the desktop is doing now for taskID, if its running turn
// belongs to that task.
func liveStep(th conversation.Thread, taskID string) string {
	if !th.Running || len(th.Steps) == 0 {
		return ""
	}
	for i := len(th.Entries) - 1; i >= 0; i-- {
		if th.Entries[i].Role == conversation.RoleUser {
			if th.Entries[i].TaskID != taskID {
				return ""
			}
			break
		}
	}
	return th.Steps[len(th.Steps)-1].Text
}

// Sessions returns recent sessions, each running one with what its desktop
// is doing right now.
func (c *Concierge) Sessions(limit int) ([]Session, error) {
	list, err := c.Store.RecentSessions(limit)
	if err != nil {
		return nil, err
	}
	threads := map[string]conversation.Thread{}
	for i, s := range list {
		if s.Status != StatusRunning {
			continue
		}
		th, ok := threads[s.DesktopID]
		if !ok {
			th = c.Deps.Transcript(s.DesktopID)
			threads[s.DesktopID] = th
		}
		list[i].LiveStep = liveStep(th, s.TaskID)
	}
	return list, nil
}

func (c *Concierge) statusText(ctx context.Context) string {
	desktops, err := c.Deps.Desktops(ctx)
	if err != nil {
		return "Could not list the desktops: " + err.Error()
	}
	if len(desktops) == 0 {
		return "There are no desktops yet. Create one on the Desktops page, then tell me what to do."
	}

	active, _ := c.Store.ActiveSessions()
	byDesktop := map[string]Session{}
	for _, s := range active {
		byDesktop[s.DesktopID] = s
	}

	var b strings.Builder
	b.WriteString("Desktops:")
	for _, d := range desktops {
		fmt.Fprintf(&b, "\n  @%s - ", d.ID)
		if s, ok := byDesktop[d.ID]; ok {
			if s.Status == StatusNeedsYou {
				fmt.Fprintf(&b, "waiting on you (#%d: %s)", s.ID, clip(s.Request, 60))
			} else {
				fmt.Fprintf(&b, "working on #%d: %s", s.ID, clip(s.Request, 60))
			}
			continue
		}
		why, _ := c.Deps.Busy(d.ID)
		switch {
		case !usable(d):
			b.WriteString(stateOf(d))
		case why != "":
			b.WriteString("busy (" + why + ")")
		case d.Status == "running":
			b.WriteString("free")
		default:
			b.WriteString(d.Status + ", free (I'll wake it)")
		}
	}
	return b.String()
}

func (c *Concierge) sessionsText() string {
	list, err := c.Store.RecentSessions(10)
	if err != nil {
		return "Could not list jobs: " + err.Error()
	}
	if len(list) == 0 {
		return "No jobs yet. Tell me what you want done."
	}
	var b strings.Builder
	b.WriteString("Recent jobs:")
	for _, s := range list {
		fmt.Fprintf(&b, "\n  #%d %s on %s: %s", s.ID, statusWord(s.Status), s.DesktopName, clip(s.Request, 60))
	}
	return b.String()
}

func statusWord(status string) string {
	switch status {
	case StatusNeedsYou:
		return "needs you"
	case StatusRunning:
		return "running"
	case StatusDone:
		return "done"
	case StatusFailed:
		return "failed"
	case StatusStopped:
		return "stopped"
	}
	return status
}

func (c *Concierge) busyText(d desktop.Desktop, why string) string {
	active, _ := c.Store.ActiveSessions()
	for _, s := range active {
		if s.DesktopID == d.ID {
			return fmt.Sprintf("%s is busy with #%d (%s). Wait for it, or send this to another desktop with @name.", d.Name, s.ID, clip(s.Request, 60))
		}
	}
	return fmt.Sprintf("%s is busy (%s). Wait for it, or send this to another desktop with @name.", d.Name, why)
}

func (c *Concierge) noFreeDesktopText(desktops []desktop.Desktop) string {
	if len(desktops) == 0 {
		return "There are no desktops yet. Create one on the Desktops page, then tell me again."
	}
	return "Every desktop is busy. I didn't start it - wait for one to finish (\"status\" shows them), or create another desktop."
}

func (c *Concierge) fail(err error) {
	log.Printf("concierge: %v", err)
	c.say(0, "error", "Something went wrong on my side - try again.")
}

// clip shortens s to n runes for a one-line summary.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
