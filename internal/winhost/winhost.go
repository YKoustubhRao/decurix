// Package winhost runs a Windows application on a private desktop of the host machine, so it has no window on
// the person's own screen, and lets the control plane see it (a stream of JPEG frames) and drive it (mouse and
// keyboard) - which is how a desktop VM can show and operate an app that is really running on the host.
//
// The Windows half is bridge.ps1, embedded here. It is spoken to over the stdin/stdout of the process that runs
// it and listens on no port, so nothing but the Manager that started it can reach the app. The package depends
// on nothing else in Flowera: how the script is run on the host is the Starter's business.
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
	"os/exec"
	"regexp"
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

// Frame is one capture of the app's window.
type Frame struct {
	JPEG []byte
	W, H int
	// Seq grows by one each time the picture actually changes.
	Seq uint64
}

// Timings, variables so tests can shorten them.
var (
	requestTimeout = 20 * time.Second
	launchTimeout  = 30 * time.Second
	frameInterval  = 120 * time.Millisecond
	// A session is only captured while somebody has touched it this recently...
	activeWindow = 8 * time.Second
	// ...and is ended, taking the app with it, once nobody has for this long.
	idleStop = 90 * time.Second
)

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// Manager owns the running sessions, one per id.
type Manager struct {
	start Starter

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager returns a Manager that runs its helper processes through start.
func NewManager(start Starter) *Manager {
	return &Manager{start: start, sessions: make(map[string]*Session)}
}

// Get returns the live session with this id.
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

// Alive reports whether a session with this id is running.
func (m *Manager) Alive(id string) bool {
	_, ok := m.Get(id)
	return ok
}

// Start launches target (with args) as session id, or returns the one already running under that id. It waits
// for the app's first window, so a returned session always has a frame; an app that exits or never opens one
// is an error rather than a blank screen.
func (m *Manager) Start(ctx context.Context, id, target, args string) (*Session, error) {
	if strings.ContainsAny(target+args, "\r\n") || strings.Contains(target, "\t") || strings.TrimSpace(target) == "" {
		return nil, errors.New("invalid application to start")
	}

	m.mu.Lock()
	if s, ok := m.sessions[id]; ok && s.Alive() {
		s.Touch()
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	name := "flowera_" + unsafeName.ReplaceAllString(id, "_")
	if len(name) > 48 {
		name = name[:48]
	}
	// The helper outlives this request, so it must not be tied to the request's context.
	cmd, err := m.start(context.Background(), name)
	if err != nil {
		return nil, err
	}
	s, err := newSession(id, cmd)
	if err != nil {
		return nil, err
	}

	resp, err := s.Do("launch\t" + target + "\t" + args)
	if err != nil {
		s.Stop()
		return nil, err
	}
	if !strings.HasPrefix(resp, "OK") {
		s.Stop()
		return nil, errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	if err := s.awaitFirstFrame(ctx); err != nil {
		s.Stop()
		return nil, err
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	go s.pump()
	return s, nil
}

// Stop ends session id and the app in it.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.Stop()
	}
}

// StopAll ends every session, for shutdown: an app on a private desktop is invisible, so it must never be left behind.
func (m *Manager) StopAll() {
	m.mu.Lock()
	all := m.sessions
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()
	for _, s := range all {
		s.Stop()
	}
}

// Session is one app running on its own private desktop.
type Session struct {
	id    string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	done  chan struct{}

	ioMu sync.Mutex // one request in flight at a time: the protocol is strictly request/response

	// The timings this session runs by, fixed when it is created.
	interval, active, idle time.Duration

	mu      sync.Mutex
	err     error
	touched time.Time
	frame   Frame
	updated chan struct{}
	stderr  bytes.Buffer

	stopOnce sync.Once
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
		lines:   make(chan string, 4),
		done:    make(chan struct{}),
		touched: time.Now(),
		updated: make(chan struct{}),

		interval: frameInterval,
		active:   activeWindow,
		idle:     idleStop,
	}
	cmd.Stderr = &lockedWriter{mu: &s.mu, buf: &s.stderr}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start host helper: %w", err)
	}

	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadString('\n')
			if line = strings.TrimSpace(line); line != "" {
				select {
				case s.lines <- line:
				case <-s.done:
					return
				}
			}
			if err != nil {
				close(s.lines)
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		s.fail(fmt.Errorf("host helper ended: %v", err))
		close(s.done)
	}()
	return s, nil
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

// Touch records that somebody is looking at the session, which keeps it capturing and keeps it alive.
func (s *Session) Touch() {
	s.mu.Lock()
	s.touched = time.Now()
	s.mu.Unlock()
}

// Watch keeps the session alive and capturing until ctx is done, for a viewer that stays connected while what it is
// showing does not change: without it a static app would look unwatched and be ended under someone's eyes.
func (s *Session) Watch(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.active / 2)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case <-t.C:
				s.Touch()
			}
		}
	}()
}

func (s *Session) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.touched)
}

// Do sends one protocol line and returns the helper's one-line answer.
func (s *Session) Do(line string) (string, error) {
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
	case resp, ok := <-s.lines:
		if !ok {
			return "", s.deadErr()
		}
		return resp, nil
	case <-s.done:
		return "", s.deadErr()
	case <-time.After(requestTimeout):
		s.fail(errors.New("the host helper stopped answering"))
		go s.Stop()
		return "", s.deadErr()
	}
}

func (s *Session) deadErr() error {
	if err := s.Err(); err != nil {
		return err
	}
	return errors.New("the host helper is not running")
}

func parseFrame(resp string) (Frame, bool) {
	f := strings.SplitN(resp, " ", 4)
	if len(f) != 4 || f[0] != "FRAME" {
		return Frame{}, false
	}
	w, err1 := strconv.Atoi(f[1])
	h, err2 := strconv.Atoi(f[2])
	jpeg, err3 := base64.StdEncoding.DecodeString(f[3])
	if err1 != nil || err2 != nil || err3 != nil || len(jpeg) == 0 {
		return Frame{}, false
	}
	return Frame{JPEG: jpeg, W: w, H: h}, true
}

// publish stores a new capture and wakes anyone waiting for one, unless the picture has not changed.
func (s *Session) publish(f Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frame.Seq > 0 && bytes.Equal(s.frame.JPEG, f.JPEG) {
		return
	}
	f.Seq = s.frame.Seq + 1
	s.frame = f
	close(s.updated)
	s.updated = make(chan struct{})
}

func (s *Session) awaitFirstFrame(ctx context.Context) error {
	deadline := time.Now().Add(launchTimeout)
	for {
		resp, err := s.Do("frame")
		if err != nil {
			return err
		}
		if f, ok := parseFrame(resp); ok {
			s.publish(f)
			return nil
		}
		if st, err := s.Do("status"); err == nil && strings.HasPrefix(st, "STATUS exited") {
			return fmt.Errorf("the app closed straight away (%s) without opening a window", strings.TrimPrefix(st, "STATUS "))
		}
		if time.Now().After(deadline) {
			return errors.New("the app did not open a window in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// pump keeps the latest frame fresh while somebody is watching, and ends the session when nobody has for a while.
func (s *Session) pump() {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		idleFor := s.idleFor()
		if idleFor > s.idle {
			s.Stop()
			return
		}
		if idleFor > s.active {
			continue
		}
		resp, err := s.Do("frame")
		if err != nil {
			return
		}
		if f, ok := parseFrame(resp); ok {
			s.publish(f)
		}
	}
}

// Latest is the newest frame, and whether there is one.
func (s *Session) Latest() (Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frame, s.frame.Seq > 0
}

// WaitFrame blocks until a frame newer than after exists, the session ends, or ctx is done.
func (s *Session) WaitFrame(ctx context.Context, after uint64) (Frame, error) {
	for {
		s.mu.Lock()
		f, ch := s.frame, s.updated
		s.mu.Unlock()
		if f.Seq > after {
			return f, nil
		}
		select {
		case <-ch:
		case <-s.done:
			return Frame{}, s.deadErr()
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
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

// Type sends characters to whatever was clicked last. Line breaks are keys (Enter), not text.
func (s *Session) Type(text string) error {
	text = strings.NewReplacer("\r", "", "\n", "").Replace(text)
	if text == "" {
		return nil
	}
	return s.send("type " + text)
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

// maxPaste bounds what one paste may type into the app.
const maxPaste = 200_000

// Paste types text into the app as if it had been typed, with each line break as an Enter. It deliberately does not go
// through the clipboard, which belongs to the person at the host: nothing they copied there is put into the app, and
// nothing the VM pasted is left on their clipboard.
func (s *Session) Paste(text string) error {
	if len([]rune(text)) > maxPaste {
		return fmt.Errorf("too much to paste (over %d characters)", maxPaste)
	}
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			if err := s.Key(13, ""); err != nil {
				return err
			}
		}
		if err := s.Type(line); err != nil {
			return err
		}
	}
	return nil
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
	s.stopOnce.Do(func() {
		// Closing stdin is what makes the helper shut the app down; asking politely first is best-effort.
		go func() {
			s.ioMu.Lock()
			io.WriteString(s.stdin, "quit\n")
			s.ioMu.Unlock()
		}()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			s.stdin.Close()
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				if s.cmd.Process != nil {
					s.cmd.Process.Kill()
				}
			}
		}
	})
}
