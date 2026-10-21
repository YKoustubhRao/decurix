// Package winhost runs a Windows application on a private desktop of the host machine, so it has no window on
// the person's own screen, and lets the control plane see it (a stream of JPEG patches) and drive it (mouse and
// keyboard) - which is how a desktop VM can show and operate an app that is really running on the host.
//
// The Windows half is bridge.ps1, embedded here. It is spoken to over the stdin/stdout of the process that runs
// it and listens on no port, so nothing but the Manager that started it can reach the app. The package depends
// on nothing else in Decurix: how the script is run on the host is the Starter's business.
//
// Starting an app is fast because nothing slow happens after the person asks for it: a helper is kept running and
// warm (compiled, with what it learned about the Start menu), an app is launched into it, and the helper - not a
// request/response loop - pushes what changes on screen as it changes, only the part of the picture that differs.
package winhost

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Script is bridge.ps1, the Windows side of the protocol. A Starter writes it somewhere the host can read.
//
//go:embed bridge.ps1
var Script []byte

// Starter returns the not-yet-started command that runs Script on the host for a private desktop called
// desktopName, with stdin and stdout left free for the Manager to wire up.
type Starter func(ctx context.Context, desktopName string) (*exec.Cmd, error)

// Update is one thing the app's window did: the picture is W x H, and JPEG is its rectangle X,Y,Rw x Rh. When Whole is
// set that rectangle is the entire picture, so a viewer that has it needs nothing before it.
type Update struct {
	W, H   int
	X, Y   int
	Rw, Rh int
	Whole  bool
	JPEG   []byte
}

// Timings, variables so tests can shorten them.
var (
	requestTimeout = 20 * time.Second
	// checkHelperTries is how many helpers in a row may die without answering a single check before Check gives up.
	checkHelperTries = 3
	// A launch answers once the program has been started, and the first launch of a browser for a VM first brings the
	// person's profile over into that VM's private one (bridge.ps1's Initialize-PrivateBrowserProfile) - hundreds of MB,
	// a minute or more on a busy host. Timed out at requestTimeout, the helper was killed mid-copy, the copy never
	// finished, and every retry (the kept app's, the person's click) met the same "stopped answering".
	launchRequestTimeout = 15 * time.Minute
	launchTimeout        = 30 * time.Second
	// How long Keep gives one start of a kept app, profile import included.
	keepStartTimeout = 20 * time.Minute
	// A session nobody is watching or driving is ended, taking the app with it, once this long has passed.
	idleStop = 90 * time.Second
	// A session started ahead of time (Manager.Start called before anyone has ever watched it - see hostAppStarter)
	// is given longer before the same rule ends it: the person who will open it has not necessarily reached the VM
	// yet, and killing it the moment idleStop passes would spend the whole point of starting it early. Once it has
	// been watched at least once, idleStop applies as usual - this only widens the window before the first look.
	prelaunchIdleStop = 20 * time.Minute
	// A warm helper that nobody has asked for is let go after this long, so a feature nobody uses holds no memory. Anything
	// that suggests an app is about to be wanted (the create dialog, a copy) starts the clock again.
	spareTTL = 2 * time.Hour
	// How often an app that has not shown a window yet is asked whether it is still running.
	statusPoll = 700 * time.Millisecond
	// A kept app (Keep) that ended is started again after this long, so the next person to open it finds it running.
	keepRelaunch = 2 * time.Second
	// A kept app that would not start is tried again after keepRetryMin, doubling up to keepRetryMax: a program that has
	// been uninstalled must not be launched in a tight loop.
	keepRetryMin = 30 * time.Second
	keepRetryMax = 30 * time.Minute
	// A kept app that closes straight away this many times running is not tried again (a program that cannot run on a
	// private desktop): until it is kept again - its desktop started again, or the app copied again.
	keepGiveUpAfter = 3
)

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// desktopName is the private desktop a session id gets: what the helper is told, and what it names an app's own
// profile after.
func desktopName(id string) string {
	name := "decurix_" + unsafeName.ReplaceAllString(id, "_")
	if len(name) > 48 {
		name = name[:48]
	}

	return name
}

// ProfileDir is where the helper keeps the profile of its own that it gives a browser or Electron app started under
// session id (bridge.ps1's Complete-Launch: $ProfileRoot\<desktop>-<base>). base is the program's name without ".exe",
// in lower case. "" when this computer has no such place (not Windows).
func ProfileDir(id, base string) string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}

	return filepath.Join(local, "Decurix", "hidden-profiles", desktopName(id)+"-"+base)
}

// Manager owns the running sessions, one per id, and one warm helper for the next.
type Manager struct {
	start Starter

	mu       sync.Mutex
	sessions map[string]*Session
	starting map[string]*startCall
	spare    *Session
	spareTmr *time.Timer
	helpers  int
	closed   bool
	warming  sync.WaitGroup // background helper starts still in flight
	// resolved remembers what a check found each program to be, so a launch never has to look it up (the slow part), and
	// stays valid when the helper that did the looking is replaced.
	resolved map[Target]resolvedProgram
	// stopping holds sessions Stop has been asked to end and that have not ended yet. A new app under the same id waits
	// for it: both would use the same private desktop, and the old one's shutdown closes every window on it.
	stopping map[string]*Session
	// kept are the apps kept running and ready (Keep), by session id.
	kept map[string]*keptApp
	// OnGiveUp, when set, is told about a kept app that is no longer tried (keepGiveUpAfter): its VM, its session id,
	// and why, in plain words. Set it before the first Keep.
	OnGiveUp func(desktop, id, reason string)
	// Reap, when set, is called by a session's Stop, after the helper was asked to end and its host command was
	// ended, with the host process ids the helper reported (its own, and the app it launched; 0 when unknown): it ends
	// what of those is still running on the host, and nothing else. Found live (2026-10-20): /api/host-gui/stop
	// answered 204 and left the helper's PowerShell and the WINWORD.EXE it started running on the person's computer -
	// a Windows program started through WSL outlives the Linux side that started it. Set it before the first Start.
	Reap func(helperPID, appPID int)
}

// keptApp is an app kept open on its private desktop until Forget, so that opening it in a VM shows it at once.
type keptApp struct {
	desktop, target, args string
	stop                  chan struct{}
	gaveUp                bool // keepRunning stopped trying it (keepGiveUpAfter): a Keep starts it again
}

// resolvedProgram is what the helper's check worked out for a target.
type resolvedProgram struct {
	exe, args, dir, own, base string
	// launcher is the web-app launcher this program stands in for (a browser opening its page), else "".
	launcher string
}

func (r resolvedProgram) line() string {
	return "launchr\t" + r.exe + "\t" + r.args + "\t" + r.dir + "\t" + r.own + "\t" + r.base + "\t" + r.launcher
}

// startCall is a Start in progress, so a second request for the same app waits for it instead of launching another.
type startCall struct {
	done chan struct{}
	s    *Session
	err  error
}

// NewManager returns a Manager that runs its helper processes through start.
func NewManager(start Starter) *Manager {
	return &Manager{start: start, sessions: make(map[string]*Session), starting: make(map[string]*startCall),
		resolved: make(map[Target]resolvedProgram), stopping: make(map[string]*Session), kept: make(map[string]*keptApp)}
}

// Get returns the running session for id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if ok && !s.Alive() {
		delete(m.sessions, id)
		return nil, false
	}
	return s, ok
}

// Alive reports whether session id is running.
func (m *Manager) Alive(id string) bool {
	_, ok := m.Get(id)
	return ok
}

// helper returns a helper process that is up and answering and has no app: the warm spare if there is one, otherwise a
// new one. Whoever takes it owns it.
func (m *Manager) helper(ctx context.Context) (*Session, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("the host app manager has been shut down")
	}
	h := m.spare
	m.spare = nil
	if m.spareTmr != nil {
		m.spareTmr.Stop()
		m.spareTmr = nil
	}
	m.helpers++
	n := m.helpers
	m.mu.Unlock()
	if h != nil && h.Alive() {
		return h, nil
	}
	if h != nil {
		h.Stop()
	}

	// The helper outlives this request, so it must not be tied to the request's context.
	cmd, err := m.start(context.Background(), fmt.Sprintf("decurix_helper_%d", n))
	if err != nil {
		return nil, err
	}
	s, err := newSession("helper", cmd)
	if err != nil {
		return nil, err
	}
	// The first answer only comes once the script has loaded and is listening.
	if _, err := s.Do("viewers 0"); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

// parsePID is the process id in a helper's answer: "OK 4242" (pid) or "OK pid=4242 ..." (launch); 0 when there is none.
func parsePID(resp string) int {
	f := strings.Fields(resp)
	if len(f) < 2 || f[0] != "OK" {
		return 0
	}
	v := strings.TrimPrefix(f[1], "pid=")
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// HostPIDs lists the host process ids of the helpers this manager runs now (sessions, the spare, and those being
// stopped): what a sweep of the host must leave alone.
func (m *Manager) HostPIDs() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int
	add := func(s *Session) {
		if s != nil && s.hostPID > 0 {
			out = append(out, s.hostPID)
		}
	}
	for _, s := range m.sessions {
		add(s)
	}
	for _, s := range m.stopping {
		add(s)
	}
	add(m.spare)
	return out
}

// keepWarm puts an unused helper back to wait for the next app, or ends it if one is already waiting.
func (m *Manager) keepWarm(h *Session) {
	if h == nil || !h.Alive() {
		return
	}
	m.mu.Lock()
	if m.closed || m.spare != nil { // shut down meanwhile, or one is already waiting: this one would only hold memory
		m.mu.Unlock()
		h.Stop()
		return
	}
	m.spare = h
	m.spareTmr = time.AfterFunc(spareTTL, func() {
		m.mu.Lock()
		if m.spare != h {
			m.mu.Unlock()
			return
		}
		m.spare, m.spareTmr = nil, nil
		m.mu.Unlock()
		h.Stop()
	})
	m.mu.Unlock()
}

// Warm makes sure a helper is up and waiting for the next app, so nobody waits for one: it starts one in the background if
// there is none, and if there is one it keeps it another spareTTL. Call it whenever an app is likely to be wanted soon - the
// control plane starting, someone choosing apps to copy, a desktop being created - and it returns at once.
func (m *Manager) Warm() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if m.spare != nil {
		if m.spareTmr != nil {
			m.spareTmr.Reset(spareTTL)
		}
		m.mu.Unlock()
		return
	}
	m.warming.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.warming.Done()
		h, err := m.helper(context.Background())
		if err != nil {
			return
		}
		m.keepWarm(h)
	}()
}

// Start launches an app on its own private desktop under session id and returns once its window has shown, or joins the
// session already running under that id.
func (m *Manager) Start(ctx context.Context, id, target, args string) (*Session, error) {
	if strings.ContainsAny(target+args, "\r\n") || strings.Contains(target, "\t") || strings.TrimSpace(target) == "" {
		return nil, errors.New("invalid application to start")
	}

	m.mu.Lock()
	for {
		// An app that is closing (the person closed it, or its viewer) is not joined: the next one starts once it has
		// gone, since both would be on the same private desktop.
		old, ok := m.sessions[id]
		if ok && old.Alive() && !old.ending() {
			old.Touch()
			m.mu.Unlock()
			return old, nil
		}
		if !ok || !old.Alive() {
			old = m.stopping[id]
		}
		if old == nil || !old.Alive() {
			break
		}
		m.mu.Unlock()
		select {
		case <-old.Done():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		m.mu.Lock()
	}
	// A viewer that loads twice, or two windows opened at once, must not launch the app twice on one desktop.
	if c, ok := m.starting[id]; ok {
		m.mu.Unlock()
		select {
		case <-c.done:
			return c.s, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &startCall{done: make(chan struct{})}
	m.starting[id] = c
	m.mu.Unlock()

	s, err := m.launch(ctx, id, target, args)

	m.mu.Lock()
	delete(m.starting, id)
	if err == nil {
		m.sessions[id] = s
	}
	m.mu.Unlock()
	c.s, c.err = s, err
	close(c.done)
	if err == nil {
		m.Warm() // the next app should find a helper waiting, as this one did
	}
	return s, err
}

func (m *Manager) launch(ctx context.Context, id, target, args string) (*Session, error) {
	s, err := m.helper(ctx)
	if err != nil {
		return nil, err
	}
	s.id = id
	name := desktopName(id)
	for _, line := range []string{"desktop " + name, "viewers 1"} {
		if resp, err := s.Do(line); err != nil || !strings.HasPrefix(resp, "OK") {
			s.Stop()
			return nil, firstErr(err, errors.New("the host helper refused to start"))
		}
	}
	s.holdWatching(true) // the picture is captured from the moment the window exists, before anybody has connected
	// Which host process the helper is, so its Stop can end that tree and nothing else (an older script does not say: 0).
	if resp, err := s.Do("pid"); err == nil {
		s.hostPID = parsePID(resp)
	}
	s.reaper = m.Reap

	resp, err := m.launchLine(s, target, args)
	if err != nil {
		s.Stop()
		return nil, err
	}
	if strings.HasPrefix(resp, "OK") {
		s.appPID = parsePID(resp) // the app this helper started: its Stop ends it if the helper's ending did not
	}
	if !strings.HasPrefix(resp, "OK") {
		// Refused before anything ran (a Store app, a missing program): the helper is as clean as it was, and now knows
		// the answer for that program, so it is kept for the next app.
		s.holdWatching(false)
		m.keepWarm(s)
		return nil, errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	if err := s.awaitFirstFrame(ctx); err != nil {
		s.Stop()
		return nil, err
	}
	s.holdWatching(false)
	s.Touch() // start the idle clock from now, not from whenever this helper process was first spawned as a spare
	go s.reap()
	return s, nil
}

// launchLine starts the program in helper s. If a check has already resolved it the helper is told exactly what to run;
// if that program has since moved, or was never checked, the helper looks it up itself.
func (m *Manager) launchLine(s *Session, target, args string) (string, error) {
	t := Target{target, args}
	m.mu.Lock()
	r, known := m.resolved[t]
	m.mu.Unlock()
	if known {
		resp, err := s.DoTimeout(r.line(), launchRequestTimeout)
		if err != nil || !strings.HasPrefix(resp, "ERR Not found") {
			return resp, err
		}
		m.mu.Lock()
		delete(m.resolved, t) // moved or uninstalled since the check; do not trust it again
		m.mu.Unlock()
	}
	return s.DoTimeout("launch\t"+target+"\t"+args, launchRequestTimeout)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// Check asks the host - without starting anything - which of targets Start would be able to run on a private desktop,
// using the same decision the helper's launch makes. The result maps each target it got an answer for to why it cannot
// run ("" when it can). A target the line protocol cannot carry is answered here, with no round trip. A helper that
// stops answering partway is replaced and the rest asked again; if helpers keep dying without answering, what was
// learned so far is returned along with the error, so a caller can keep it. The
// helper it used stays warm, and what it worked out makes the launch that follows instant.
func (m *Manager) Check(ctx context.Context, targets []Target) (map[Target]string, error) {
	verdicts := make(map[Target]string, len(targets))
	var ask []Target
	for _, t := range targets {
		if strings.ContainsAny(t.Target+t.Args, "\r\n") || strings.Contains(t.Target, "\t") || strings.TrimSpace(t.Target) == "" {
			verdicts[t] = "invalid application"
			continue
		}
		ask = append(ask, t)
	}
	if len(ask) == 0 {
		return verdicts, nil
	}

	h, err := m.helper(ctx)
	if err != nil {
		return verdicts, err
	}
	// A helper that dies partway (found live 2026-10-20: right after a control-plane start, with kept apps starting
	// alongside, one stopped answering after 56 of 119 checks) is replaced and the rest asked again: its death says
	// nothing about the targets it never got to. Only helpers that die without answering anything count against the
	// limit, so a host that cannot keep one alive still ends in an error.
	barren := 0
	answered := false
	for i := 0; i < len(ask); i++ {
		t := ask[i]
		if err := ctx.Err(); err != nil {
			h.Stop()
			return verdicts, err
		}
		resp, err := h.Do("check\t" + t.Target + "\t" + t.Args)
		if err != nil {
			h.Stop()
			if !answered {
				barren++
			}
			if barren >= checkHelperTries || ctx.Err() != nil {
				return verdicts, err
			}
			log.Printf("winhost: the check helper stopped after %d of %d targets (%v); asking the rest on a new one", i, len(ask), err)
			if h, err = m.helper(ctx); err != nil {
				return verdicts, err
			}
			answered = false
			i-- // ask this target again
			continue
		}
		answered = true
		if strings.HasPrefix(resp, "OK") {
			verdicts[t] = ""
			// Six fields from a helper that predates the launcher field, seven since.
			if f := strings.Split(resp, "\t"); len(f) >= 6 && f[1] != "" {
				r := resolvedProgram{exe: f[1], args: f[2], dir: f[3], own: f[4], base: f[5]}
				if len(f) > 6 {
					r.launcher = f[6]
				}
				m.mu.Lock()
				m.resolved[t] = r
				m.mu.Unlock()
			}
			continue
		}
		reason := strings.TrimSpace(strings.TrimPrefix(resp, "ERR"))
		if reason == "" {
			reason = "the host could not run it"
		}
		verdicts[t] = reason
	}
	m.keepWarm(h)
	return verdicts, nil
}

// appPage is the address in a browser's --app= argument.
var appPage = regexp.MustCompile(`--app=(\S+)`)

// LocalPage is the page a checked target only opens, when that target is a web app's launcher (a small program that
// starts the app's server and opens its page in a browser) and the page is on this computer - "" otherwise, and for a
// target no check has resolved. Such an app is a web page, which a VM's own browser can show and an agent can read
// field by field, where the hidden host browser's picture is all a worker would get.
func (m *Manager) LocalPage(target, args string) string {
	m.mu.Lock()
	r, ok := m.resolved[Target{target, args}]
	m.mu.Unlock()
	if !ok || r.launcher == "" {
		return ""
	}
	match := appPage.FindStringSubmatch(r.args)
	if match == nil {
		return ""
	}
	u, err := url.Parse(strings.Trim(match[1], `"`))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return u.String()
	}
	return ""
}

// maxThumbnail bounds one small picture from the helper.
const maxThumbnail = 8 << 20

// Target is one program a launcher would ask the host to run.
type Target struct {
	Target, Args string
}

// waitIdle returns once no helper is still being started in the background, for tests that change the timings.
func (m *Manager) waitIdle() { m.warming.Wait() }

// Stop ends session id and the app in it. A kept app (Keep) is started again, fresh, a moment later.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	if s != nil {
		m.stopping[id] = s
	}
	m.mu.Unlock()
	if s != nil {
		s.Stop()
		m.mu.Lock()
		if m.stopping[id] == s {
			delete(m.stopping, id)
		}
		m.mu.Unlock()
	}
}

// Keep keeps an app running and ready under session id until Forget: it is started now if it is not already, and again
// whenever it ends - the person closed it, closed the window showing it, or it crashed - so opening it in a VM shows it
// at once instead of starting it on the host while somebody watches. A kept app nobody has looked at yet is never ended
// for being unwatched. desktop is the VM it was copied to (ForgetDesktop).
func (m *Manager) Keep(desktop, id, target, args string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if k, ok := m.kept[id]; ok {
		if k.target == target && k.args == args && !k.gaveUp {
			m.mu.Unlock()
			return
		}
		// Copied again as another program, or kept again after it was given up on: keep this one instead.
		close(k.stop)
	}
	k := &keptApp{desktop: desktop, target: target, args: args, stop: make(chan struct{})}
	m.kept[id] = k
	m.mu.Unlock()
	go m.keepRunning(id, k)
}

// Forget stops keeping app id ready, and ends it.
func (m *Manager) Forget(id string) {
	m.mu.Lock()
	k, ok := m.kept[id]
	if ok {
		delete(m.kept, id)
		close(k.stop)
	}
	m.mu.Unlock()
	if ok {
		m.Stop(id)
	}
}

// KeptFor lists the session ids of the apps kept ready for one VM (those given up on included), sorted.
func (m *Manager) KeptFor(desktop string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, k := range m.kept {
		if k.desktop == desktop {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// ForgetDesktop forgets every app kept ready for one VM, for when it is deleted or stopped.
func (m *Manager) ForgetDesktop(desktop string) {
	m.mu.Lock()
	var ids []string
	for id, k := range m.kept {
		if k.desktop == desktop {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Forget(id)
	}
}

// closedAtOnce is the error a launch gives for an app that exits before it ever shows a window.
const closedAtOnce = "closed straight away"

// keepRunning is Keep's loop for one app: start it, wait for it to end, start it again. An app that closes straight away
// keepGiveUpAfter times running is given up on (OnGiveUp) until it is kept again.
func (m *Manager) keepRunning(id string, k *keptApp) {
	var backoff time.Duration
	closedRunning := 0
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-k.stop:
			return false
		case <-t.C:
			return true
		}
	}
	for {
		select {
		case <-k.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), keepStartTimeout)
		s, err := m.Start(ctx, id, k.target, k.args)
		cancel()
		if err != nil {
			m.mu.Lock()
			closed := m.closed
			m.mu.Unlock()
			if closed {
				return
			}
			if strings.Contains(err.Error(), closedAtOnce) {
				closedRunning++
			} else {
				closedRunning = 0
			}
			if closedRunning >= keepGiveUpAfter {
				m.mu.Lock()
				if m.kept[id] == k {
					k.gaveUp = true
				}
				onGiveUp := m.OnGiveUp
				m.mu.Unlock()
				log.Printf("host apps: not keeping %s ready any more: it closed straight away %d times (%v)", id, closedRunning, err)
				if onGiveUp != nil {
					onGiveUp(k.desktop, id, fmt.Sprintf("It cannot run here: on this computer it closed straight away each "+
						"of the %d times it was started, without opening a window.", closedRunning))
				}
				return
			}
			backoff = min(max(2*backoff, keepRetryMin), keepRetryMax)
			log.Printf("host apps: could not keep %s ready (trying again in %s): %v", id, backoff, err)
			if !wait(backoff) {
				return
			}
			continue
		}
		backoff = 0
		closedRunning = 0
		s.setKept(true)
		select {
		case <-k.stop:
			return
		case <-s.Done():
		}
		if !wait(keepRelaunch) {
			return
		}
	}
}

// StopAll ends every session and the warm helper, for shutdown: an app on a private desktop is invisible, so it must
// never be left behind.
func (m *Manager) StopAll() {
	m.mu.Lock()
	m.closed = true
	for id, k := range m.kept {
		close(k.stop)
		delete(m.kept, id)
	}
	all := m.sessions
	m.sessions = make(map[string]*Session)
	spare := m.spare
	m.spare = nil
	if m.spareTmr != nil {
		m.spareTmr.Stop()
		m.spareTmr = nil
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Stop()
	}
	if spare != nil {
		spare.Stop()
	}
}

// Session is one helper process, and - once an app is launched into it - that app on its own private desktop.
type Session struct {
	id    string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// replies carries the helper's one-line answers to commands, in order; pictures and events never appear on it.
	replies chan string
	done    chan struct{}

	ioMu sync.Mutex // one command in flight at a time: answers are matched to commands by order

	mu          sync.Mutex
	err         error
	touched     time.Time
	everWatched bool
	kept        bool // kept ready by Manager.Keep: never ended for being unwatched before anybody has looked
	stopped     bool // Stop has been called
	stderr      bytes.Buffer
	viewers     map[*Viewer]struct{}
	holding     bool
	w, h        int
	first       chan struct{}
	gotFirst    bool
	appGone     chan struct{}
	goneOnce    sync.Once
	stopOnce    sync.Once
	onEvent     func(string) // see OnEvent
	keyedSet    bool         // SetKeyedText has told the helper how this app takes text
	keyedText   bool

	// The last small picture the helper made (see Thumbnail): what it just sent, and the one kept with its width.
	thumbMu    sync.Mutex
	thumbSent  []byte
	thumbKept  []byte
	thumbWidth int

	// opens are the opens the helper is doing behind its command loop, by ticket (see open).
	opens map[string]chan string

	// hostPID is the helper's own process on the host and appPID the app it launched (0 when not known); reap is the
	// manager's Reap (as reaper), called by Stop with them.
	hostPID, appPID int
	reaper          func(helperPID, appPID int)
}

func newSession(id string, cmd *exec.Cmd) (*Session, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &Session{
		id:      id,
		cmd:     cmd,
		stdin:   stdin,
		replies: make(chan string, 4),
		done:    make(chan struct{}),
		touched: time.Now(),
		viewers: make(map[*Viewer]struct{}),
		first:   make(chan struct{}),
		appGone: make(chan struct{}),
	}
	cmd.Stderr = &lockedWriter{mu: &s.mu, buf: &s.stderr}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start host helper: %w", err)
	}

	go s.read(bufio.NewReaderSize(stdout, 1<<20))
	go func() {
		err := cmd.Wait()
		// The client that ran the helper has exited; the container it ran it in may not have.
		endHost(cmd)
		s.fail(fmt.Errorf("host helper ended: %v", err))
		close(s.done)
	}()
	return s, nil
}

// read splits what the helper says into answers (lines, in order), pictures (a header line and the JPEG bytes that follow it)
// and events (a line that starts with EVENT).
func (s *Session) read(r *bufio.Reader) {
	defer close(s.replies)
	for {
		line, err := r.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			switch {
			case strings.HasPrefix(line, "FRAME "):
				u, ok := parseFrameHeader(line)
				if !ok {
					s.fail(fmt.Errorf("the host helper sent a picture header the control plane could not read: %q", truncate(line, 80)))
					go s.Stop() // the stream cannot be trusted from here on, and a helper writing into a pipe nobody reads would block forever
					return
				}
				u.JPEG = make([]byte, u.n)
				if _, err := io.ReadFull(r, u.JPEG); err != nil {
					return
				}
				s.publish(u.Update)
			case strings.HasPrefix(line, "THUMB "):
				// "THUMB <w> <h> <n>" is followed by the picture; "THUMB same" by nothing. Either is the answer to a
				// thumb command, so the line goes on to whoever asked.
				if f := strings.Fields(line); len(f) == 4 {
					n, convErr := strconv.Atoi(f[3])
					if convErr != nil || n <= 0 || n > maxThumbnail {
						s.fail(fmt.Errorf("the host helper sent a thumbnail header the control plane could not read: %q", truncate(line, 80)))
						go s.Stop()
						return
					}
					data := make([]byte, n)
					if _, err := io.ReadFull(r, data); err != nil {
						return
					}
					s.mu.Lock()
					s.thumbSent = data
					s.mu.Unlock()
				}
				select {
				case s.replies <- line:
				case <-s.done:
					return
				}
			case strings.HasPrefix(line, "EVENT "):
				if strings.TrimPrefix(line, "EVENT ") == "closed" {
					s.markAppGone()
				}
			default:
				select {
				case s.replies <- line:
				case <-s.done:
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

type frameHeader struct {
	Update
	n int
}

// parseFrameHeader reads "FRAME <W> <H> <x> <y> <w> <h> <whole> <n>".
func parseFrameHeader(line string) (frameHeader, bool) {
	f := strings.Fields(line)
	if len(f) != 9 || f[0] != "FRAME" {
		return frameHeader{}, false
	}
	var v [8]int
	for i := range v {
		n, err := strconv.Atoi(f[i+1])
		if err != nil || n < 0 {
			return frameHeader{}, false
		}
		v[i] = n
	}
	h := frameHeader{Update: Update{W: v[0], H: v[1], X: v[2], Y: v[3], Rw: v[4], Rh: v[5], Whole: v[6] == 1}, n: v[7]}
	const maxJPEG = 32 << 20
	if h.n <= 0 || h.n > maxJPEG || h.W <= 0 || h.H <= 0 || h.X+h.Rw > h.W || h.Y+h.Rh > h.H || h.Rw <= 0 || h.Rh <= 0 {
		return frameHeader{}, false
	}
	return h, true
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// lockedWriter keeps the tail of the helper's stderr for error messages.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() < 4096 {
		w.buf.Write(p)
	}
	return len(p), nil
}

// ID is the id the session was started under.
func (s *Session) ID() string { return s.id }

// Alive reports whether the helper is still running.
func (s *Session) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Done is closed when the helper has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// AppClosed is closed when the app has no window left, which is how a viewer learns the person closed it from inside.
func (s *Session) AppClosed() <-chan struct{} { return s.appGone }

func (s *Session) markAppGone() {
	s.goneOnce.Do(func() {
		close(s.appGone)
		go s.Stop() // there is nothing left to show, and the helper has nothing left to do
	})
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Err is why the session ended, if it has.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Size is the size of the app's picture as last shown, or 0, 0 before anything has been.
func (s *Session) Size() (w, h int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w, s.h
}

func (s *Session) setKept(on bool) {
	s.mu.Lock()
	s.kept = on
	s.mu.Unlock()
}

// ending reports whether the session is on its way out: its app has closed, or Stop has been called.
func (s *Session) ending() bool {
	select {
	case <-s.appGone:
		return true
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// Touch records that somebody is using the session, which keeps it alive.
func (s *Session) Touch() {
	s.mu.Lock()
	s.touched = time.Now()
	s.mu.Unlock()
}

// reap ends the session when nobody has watched or driven it for idleStop.
func (s *Session) reap() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.mu.Lock()
		unwatched := len(s.viewers) == 0 && !s.holding
		idle := time.Since(s.touched)
		threshold := idleStop
		if !s.everWatched {
			threshold = prelaunchIdleStop
		}
		waiting := s.kept && !s.everWatched
		s.mu.Unlock()
		if unwatched && idle > threshold && !waiting {
			s.Stop()
			return
		}
	}
}

func (s *Session) deadErr() error {
	if err := s.Err(); err != nil {
		s.mu.Lock()
		tail := strings.TrimSpace(s.stderr.String())
		s.mu.Unlock()
		if tail != "" {
			return fmt.Errorf("%w (%s)", err, truncate(tail, 300))
		}
		return err
	}
	return errors.New("the host helper is not running")
}

// Do sends one protocol line and returns the helper's one-line answer.
func (s *Session) Do(line string) (string, error) {
	return s.DoTimeout(line, requestTimeout)
}

// DoTimeout is Do for a command that may take longer than requestTimeout to answer.
func (s *Session) DoTimeout(line string, timeout time.Duration) (string, error) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if !s.Alive() {
		return "", s.deadErr()
	}
	if _, err := io.WriteString(s.stdin, line+"\n"); err != nil {
		s.fail(err)
		return "", s.deadErr()
	}
	select {
	case resp, ok := <-s.replies:
		if !ok {
			return "", s.deadErr()
		}
		return resp, nil
	case <-s.done:
		return "", s.deadErr()
	case <-time.After(timeout):
		verb, _, _ := strings.Cut(strings.ReplaceAll(line, "\t", " "), " ")
		s.fail(fmt.Errorf("the host helper stopped answering (%q took over %s)", verb, timeout))
		go s.Stop()
		return "", s.deadErr()
	}
}

// holdWatching asks the helper to capture while the app starts, before any viewer has connected to it.
func (s *Session) holdWatching(on bool) {
	s.mu.Lock()
	s.holding = on
	s.mu.Unlock()
	s.syncViewers()
}

// syncViewers tells the helper how many people are watching, which is all it needs to know to capture or to rest.
func (s *Session) syncViewers() {
	s.mu.Lock()
	n := len(s.viewers)
	if s.holding {
		n++
	}
	s.mu.Unlock()
	s.Do("viewers " + strconv.Itoa(n))
}

func (s *Session) awaitFirstFrame(ctx context.Context) error {
	deadline := time.NewTimer(launchTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(statusPoll)
	defer poll.Stop()
	for {
		select {
		case <-s.first:
			return nil
		case <-s.appGone:
			return errors.New("the app closed without showing a window")
		case <-s.done:
			return s.deadErr()
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("the app did not open a window in time")
		case <-poll.C:
			// An app that dies on start never gets a window; say so instead of waiting out the clock.
			if st, err := s.Do("status"); err == nil && strings.HasPrefix(st, "STATUS exited") {
				select {
				case <-s.first:
					return nil
				default:
				}
				return fmt.Errorf("the app closed straight away (%s) without opening a window", strings.TrimPrefix(st, "STATUS "))
			}
		}
	}
}

// Viewer is somebody watching the app: it receives every Update from a whole picture onwards.
type Viewer struct {
	// C carries the app's pictures. It is closed when the viewer is.
	C <-chan Update

	s       *Session
	ch      chan Update
	waiting bool // has not yet had a whole picture, so patches are of no use to it
	once    sync.Once
}

// Watch adds a viewer. Its first Update is a whole picture; the helper is asked for one now, so it arrives at once even
// if the app is not changing. Close it when the viewer goes away.
func (s *Session) Watch() *Viewer {
	ch := make(chan Update, 32)
	v := &Viewer{C: ch, s: s, ch: ch, waiting: true}
	s.mu.Lock()
	s.viewers[v] = struct{}{}
	s.touched = time.Now()
	s.everWatched = true
	s.mu.Unlock()
	s.syncViewers()
	go s.Do("keyframe")
	return v
}

// Close removes the viewer.
func (v *Viewer) Close() {
	v.once.Do(func() {
		v.s.mu.Lock()
		delete(v.s.viewers, v)
		v.s.touched = time.Now()
		v.s.mu.Unlock()
		close(v.ch)
		if v.s.Alive() {
			v.s.syncViewers()
		}
	})
}

// publish hands an Update to every viewer. A viewer that cannot keep up loses its backlog and waits for the next whole
// picture (asked for at once), rather than drawing patches on top of a picture it never got.
func (s *Session) publish(u Update) {
	s.mu.Lock()
	if u.Whole {
		s.w, s.h = u.W, u.H
		if !s.gotFirst {
			s.gotFirst = true
			close(s.first)
		}
	}
	resync := false
	for v := range s.viewers {
		if v.waiting && !u.Whole {
			continue
		}
		select {
		case v.ch <- u:
			if u.Whole {
				v.waiting = false
			}
		default:
			if u.Whole {
				for len(v.ch) > 0 {
					<-v.ch
				}
				v.ch <- u
				v.waiting = false
			} else {
				for len(v.ch) > 0 {
					<-v.ch
				}
				v.waiting = true
				resync = true
			}
		}
	}
	s.mu.Unlock()
	if resync {
		go s.Do("keyframe") // never from the reader: Do waits for answers that only the reader can deliver
	}
}

// send runs one input command; an unrecognised answer is an error, "OK" is success.
func (s *Session) send(line string) error {
	s.Touch()
	resp, err := s.Do(line)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	return nil
}

// validMods reports whether mods is only the letters c (Ctrl), s (Shift) and a (Alt), each at most once.
func validMods(mods string) bool {
	seen := map[rune]bool{}
	for _, r := range mods {
		if (r != 'c' && r != 's' && r != 'a') || seen[r] {
			return false
		}
		seen[r] = true
	}
	return true
}

// Mouse sends "move", "down" or "up" at a position in frame pixels, with any of the modifiers c, s, a held.
func (s *Session) Mouse(kind string, x, y int, right bool, mods string) error {
	switch kind {
	case "move", "down", "up":
	default:
		return fmt.Errorf("unknown mouse action %q", kind)
	}
	if !validMods(mods) {
		return fmt.Errorf("invalid modifiers %q", mods)
	}
	line := fmt.Sprintf("%s %d %d", kind, x, y)
	if right {
		line += " right"
	}
	if mods != "" {
		line += " +" + mods
	}
	return s.send(line)
}

// Wheel scrolls at a position; positive delta is up.
func (s *Session) Wheel(x, y, delta int) error {
	return s.send(fmt.Sprintf("wheel %d %d %d", x, y, delta))
}

// Type sends characters to whatever was clicked last. A line break in the text is sent as a line break of the text
// ("newline"), which the helper gives the app the way that app takes one (see SetKeyedText).
func (s *Session) Type(text string) error {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		line = strings.ReplaceAll(line, "\r", "")
		if i > 0 {
			if err := s.send("newline"); err != nil {
				return err
			}
		}
		if line == "" {
			continue
		}
		if err := s.send("type " + line); err != nil {
			return err
		}
	}
	return nil
}

// SetKeyedText says how the app takes typed text. An app whose controls Windows describes (Office) takes characters
// as characters: a key going down is turned into a character by the app itself, after anything already queued, so
// spaces and line breaks sent as keys landed among the next letters and twice ("replacemen tlamp"). An app that draws
// its own screen (TallyPrime) dropped spaces sent as characters, and takes them as keys. Sent only when it changes.
func (s *Session) SetKeyedText(keyed bool) {
	s.mu.Lock()
	if s.keyedSet && s.keyedText == keyed {
		s.mu.Unlock()
		return
	}
	s.keyedSet, s.keyedText = true, keyed
	s.mu.Unlock()
	if keyed {
		_ = s.send("keyed 1")
	} else {
		_ = s.send("keyed 0")
	}
}

// Key sends one virtual key (Enter is 13, Backspace 8, and so on) with any of the modifiers c, s, a held.
func (s *Session) Key(vk int, mods string) error {
	if vk <= 0 || vk > 255 {
		return fmt.Errorf("invalid key %d", vk)
	}
	if !validMods(mods) {
		return fmt.Errorf("invalid modifiers %q", mods)
	}
	line := fmt.Sprintf("key %d", vk)
	if mods != "" {
		line += " " + mods
	}
	return s.send(line)
}

// Press presses the named control at (x, y) of the picture through UI Automation (Invoke, Select, Toggle or Expand)
// and returns its name; an error says nothing there could be pressed that way, and the caller clicks instead.
func (s *Session) Press(x, y int) (string, error) {
	s.Touch()
	resp, err := s.Do(fmt.Sprintf("press %d %d", x, y))
	if err != nil {
		return "", err
	}
	if name, ok := strings.CutPrefix(resp, "PRESSED "); ok {
		return name, nil
	}
	if name, ok := strings.CutPrefix(resp, "PENDING "); ok {
		return name, nil // still being handled (a dialog it opened is up): it was pressed
	}
	return "", errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
}

// SetValue puts text into the single-line field at (x, y) of the picture through UI Automation, gives it the keyboard,
// and returns what the field holds afterwards. Typed key by key, a field's text raced the app: a key overtook the last
// characters ("D20" went to D2), and spaces were dropped or doubled. A field that will not be set this way (a
// document, a field that takes no value) answers an error, and the caller types instead. One line only.
func (s *Session) SetValue(x, y int, text string, appendText bool) (string, error) {
	if strings.ContainsAny(text, "\r\n") {
		return "", errors.New("one line only")
	}
	s.Touch()
	mode := "set"
	if appendText {
		mode = "add"
	}
	resp, err := s.Do(fmt.Sprintf("setvalue %d %d %s %s", x, y, mode, text))
	if err != nil {
		return "", err
	}
	if value, ok := strings.CutPrefix(resp, "SET "); ok {
		return value, nil
	}
	if resp == "SET" {
		return "", nil
	}
	return "", errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
}

// SetFocus gives the keyboard to the control at (x, y) of the picture through UI Automation. A click posted to a dialog
// does not move its focus, so text typed after it went to whichever field had the keyboard (found live: a Save As
// file name typed into the "Save as type" box, which changed the type). Best effort: a control that cannot take the
// keyboard is left as it is.
func (s *Session) SetFocus(x, y int) error {
	return s.send(fmt.Sprintf("setfocus %d %d", x, y))
}

// Choose picks, in the list control at (x, y) of the picture (a drop-down such as Save As's "Save as type"), the one
// entry named text - exactly, else the only one starting with it, else the only one containing it - through UI
// Automation, and returns the entry's name. Keys and clicks posted to a Windows dialog do not move its keyboard focus,
// so a drop-down cannot be chosen from with them (found live: the typed words went to the dialog's folder pane).
func (s *Session) Choose(x, y int, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return "", errors.New("choose needs the entry's name, on one line")
	}
	s.Touch()
	resp, err := s.Do(fmt.Sprintf("choose %d %d %s", x, y, text))
	if err != nil {
		return "", err
	}
	name, ok := strings.CutPrefix(resp, "CHOSE ")
	if !ok {
		return "", errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	return name, nil
}

// maxPaste bounds what one paste may type into the app.
const maxPaste = 200_000

// Paste types text into the app as if it had been typed, with each line break as an Enter. It deliberately does not go
// through the clipboard, which belongs to the person at the host: nothing they copied there is put into the app, and
// nothing the VM pasted is left on their clipboard.
func (s *Session) Paste(text string) error {
	if len([]rune(text)) > maxPaste {
		return fmt.Errorf("too much to paste (over %d characters)", maxPaste)
	}
	return s.Type(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text))
}

// Copy presses Ctrl+C (vk 67) or Ctrl+X (vk 88) in the app and returns what it put on the clipboard, or "" if nothing
// changed. The host clipboard is read only here, right after an explicit copy, and what was on it before is put back.
func (s *Session) Copy(vk int) (string, error) {
	if vk != 67 && vk != 88 {
		return "", fmt.Errorf("copy is Ctrl+C or Ctrl+X, not key %d", vk)
	}
	s.Touch()
	resp, err := s.Do(fmt.Sprintf("copy %d", vk))
	if err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(resp, "CLIP")
	if !ok {
		return "", errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", nil
	}
	text, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return "", errors.New("the host sent a clipboard the control plane could not read")
	}
	return string(text), nil
}

// Stop ends the session: the helper closes the app and everything it opened, then exits.
func (s *Session) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.stopOnce.Do(func() {
		// Closing stdin is what makes the helper shut the app down; asking politely first is best-effort.
		go func() {
			s.ioMu.Lock()
			io.WriteString(s.stdin, "quit\n")
			s.ioMu.Unlock()
		}()
		select {
		case <-s.done:
		case <-time.After(6 * time.Second):
			s.stdin.Close()
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				if s.cmd.Process != nil {
					s.cmd.Process.Kill()
				}
			}
		}
		// Whatever ended it, what runs the helper on the host is ended too: when that is a container started by a
		// client that has died, the client's exit says nothing about the container, which kept the helper's stdin open
		// and so the helper alive (found live, 2026-10-20: a dozen of them in three minutes).
		endHost(s.cmd)
		// And what it started on the host, by its own process ids: the helper and the app it launched, if either is
		// still there (Manager.Reap ends only those, after checking they are what they were).
		if s.reaper != nil && (s.hostPID > 0 || s.appPID > 0) {
			s.reaper(s.hostPID, s.appPID)
		}
	})
}

// endHost runs a command's Cancel when it has one: for a host hop it removes the hop's container (desktop.HopCommand),
// for an ordinary command it kills the process. A command that has already finished is left as it is.
func endHost(cmd *exec.Cmd) {
	if cmd != nil && cmd.Cancel != nil && cmd.Process != nil {
		_ = cmd.Cancel()
	}
}
