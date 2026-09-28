// Package phoneagent is what `decurix --phone` runs on an Android phone, in
// Termux: it keeps one WebSocket open to a control plane (internal/phonehost
// is the other end), lists the phone's apps when asked, and opens an app on a
// virtual display of its own - through ADB to the phone itself and scrcpy's
// server - streaming the display's video back and injecting the viewer's
// touches and keys. The app runs on the phone; the phone's own screen can
// stay off.
//
// Depends on nothing internal but wsconn and phonehost (for the protocol).
package phoneagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YKoustubhRao/decurix/internal/phonehost"
	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// DeviceCookie is the cookie a paired device presents (internal/lanaccess).
const DeviceCookie = "decurix_lan_device"

// Config is what the agent needs to run.
type Config struct {
	// Base is the control plane's address, e.g. https://x.trycloudflare.com.
	Base string
	// Token is this phone's paired-device token.
	Token string
	// Serial picks the ADB device ("" when there is one).
	Serial string
	// Dir keeps the scrcpy-server download.
	Dir string
	// Logf reports what the agent is doing.
	Logf func(format string, args ...any)
}

// ErrRevoked is a control plane that no longer accepts this phone's pairing.
var ErrRevoked = errors.New("this phone's pairing was revoked, or the link is for another Decurix; pair it again")

// Run keeps the phone linked until ctx ends, reconnecting after a drop with a
// doubling pause (2 s up to a minute). It returns only for ctx or ErrRevoked.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}

	server, err := ensureServer(ctx, cfg.Dir)
	if err != nil {
		return err
	}

	backoff := 2 * time.Second

	for {
		started := time.Now()
		err := linkOnce(ctx, cfg, server)

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrRevoked) {
			return err
		}

		if time.Since(started) > time.Minute {
			backoff = 2 * time.Second
		}

		cfg.Logf("link down (%v); trying again in %s", err, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, time.Minute)
	}
}

// agent is one live link.
type agent struct {
	cfg  Config
	conn *wsconn.Conn

	mu       sync.Mutex
	sessions map[uint32]*session

	// sdk is the phone's Android API level; kick wakes the tree reader.
	sdk  int
	kick chan struct{}
}

// linkOnce connects, serves the link until it drops, and closes every session.
func linkOnce(ctx context.Context, cfg Config, server string) error {
	if err := pushServer(ctx, cfg.Serial, server); err != nil {
		// A restart or a new network drops ADB's connection; bring it back without asking.
		if _, rerr := Reconnect(ctx); rerr != nil {
			return rerr
		}
		if err := pushServer(ctx, cfg.Serial, server); err != nil {
			return err
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := wsconn.Dial(dialCtx, strings.TrimRight(cfg.Base, "/")+"/api/phones/connect",
		http.Header{"Cookie": {DeviceCookie + "=" + cfg.Token}})
	cancel()
	if err != nil {
		// The gate answers a pairing it does not know with its code page, not a 101.
		if strings.Contains(err.Error(), "answered 200") || strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
			return ErrRevoked
		}
		return err
	}
	defer conn.Close()

	a := &agent{cfg: cfg, conn: conn, sessions: make(map[uint32]*session), kick: make(chan struct{}, 1)}
	defer a.closeAll()

	model, android := a.describe(ctx)
	a.sdk = a.apiLevel(ctx)

	treeCtx, treeCancel := context.WithCancel(ctx)
	defer treeCancel()
	if a.sdk >= minTreeSDK {
		go a.treeLoop(treeCtx)
	}
	if err := a.send(phonehost.Message{Op: phonehost.OpHello, Model: model, Android: android}); err != nil {
		return err
	}

	cfg.Logf("linked to %s", cfg.Base)

	// The control plane pings every 25 s; a minute and a half of silence is a dead link.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	for {
		op, msg, err := conn.Read(90 * time.Second)
		if err != nil {
			return err
		}
		if op != wsconn.OpText {
			continue
		}

		var m phonehost.Message
		if json.Unmarshal(msg, &m) != nil {
			continue
		}

		a.handle(ctx, m)
	}
}

// handle takes one message from the control plane; slow work runs in its own goroutine.
func (a *agent) handle(ctx context.Context, m phonehost.Message) {
	switch m.Op {
	case phonehost.OpApps:
		go func() {
			apps, err := listApps(ctx, a.cfg.Serial)
			if err != nil {
				a.send(phonehost.Message{Op: phonehost.OpError, Req: m.Req, Error: err.Error()})
				return
			}
			a.send(phonehost.Message{Op: phonehost.OpApps, Req: m.Req, Apps: apps})
		}()

	case phonehost.OpOpen:
		go a.open(ctx, m)

	case phonehost.OpInput:
		a.mu.Lock()
		s := a.sessions[m.Session]
		a.mu.Unlock()
		if s != nil && m.Input != nil {
			s.input(*m.Input)
			s.lastInput.Store(time.Now().UnixNano())
			a.wakeTree()
		}

	case phonehost.OpClose:
		a.mu.Lock()
		s := a.sessions[m.Session]
		a.mu.Unlock()
		if s != nil {
			s.stop()
		}
	}
}

// send writes one JSON message to the control plane.
func (a *agent) send(m phonehost.Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return a.conn.Write(wsconn.OpText, raw)
}

// describe reads the phone's model and Android version, for the dashboard.
func (a *agent) describe(ctx context.Context) (model, android string) {
	get := func(prop string) string {
		out, err := adbCommand(ctx, a.cfg.Serial, "shell", "getprop", prop).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	return get("ro.product.model"), get("ro.build.version.release")
}

// apiLevel reads the phone's Android API level (0 when it cannot be read).
func (a *agent) apiLevel(ctx context.Context) int {
	out, err := adbCommand(ctx, a.cfg.Serial, "shell", "getprop", "ro.build.version.sdk").Output()
	if err != nil {
		return 0
	}

	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))

	return n
}

// wakeTree asks the tree reader to read the screens again soon.
func (a *agent) wakeTree() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// treeLoop reads what every open app's display shows and sends each session
// its controls when they changed. One dump serves every session: it covers
// every display, and Android allows one reader at a time anyway. It reads
// soon after input (a moment later, so a burst of typing is one read), often
// while someone is using an app, and seldom otherwise.
func (a *agent) treeLoop(ctx context.Context) {
	var lastInput time.Time

	for {
		live := a.streaming()

		wait := treeIdle
		if time.Since(lastInput) < treeActiveFor {
			wait = treeActive
		}
		if len(live) == 0 {
			wait = time.Hour
		}

		select {
		case <-ctx.Done():
			return
		case <-a.kick:
			lastInput = time.Now()
			// Let a burst of input finish before reading what it did.
			for {
				select {
				case <-ctx.Done():
					return
				case <-a.kick:
					continue
				case <-time.After(treeAfterInput):
				}
				break
			}
		case <-time.After(wait):
		}

		live = a.streaming()
		if len(live) == 0 {
			continue
		}

		started := time.Now()
		raw, err := dumpUI(ctx, a.cfg.Serial)
		if ctx.Err() != nil {
			return
		}

		var dump *xmlDisplays
		if err == nil {
			dump, err = parseDump(raw)
		}
		if err != nil {
			// An app that never settles cannot be read this time; the next
			// read may. The viewer keeps what it had.
			a.cfg.Logf("reading the apps' controls: %v", err)
			continue
		}

		for _, s := range live {
			s.sendTree(a, dump, started)
		}
	}
}

// streaming is every session whose display is up.
func (a *agent) streaming() []*session {
	a.mu.Lock()
	defer a.mu.Unlock()

	var out []*session
	for _, s := range a.sessions {
		if s.streaming.Load() {
			out = append(out, s)
		}
	}

	return out
}

func (a *agent) closeAll() {
	a.mu.Lock()
	list := make([]*session, 0, len(a.sessions))
	for _, s := range a.sessions {
		list = append(list, s)
	}
	a.mu.Unlock()

	for _, s := range list {
		s.stop()
	}
}

// pushServer copies the pinned server jar to the phone. It is small, and
// pushing it on every link makes a phone that was reset or re-paired work
// without a separate step.
func pushServer(ctx context.Context, serial, server string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	out, err := adbCommand(ctx, serial, "push", server, serverOnPhone).CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb cannot reach the phone (%s); is Wireless debugging paired and connected? %s",
			err, strings.TrimSpace(string(out)))
	}

	return nil
}

// listApps asks the server for the phone's launchable apps.
func listApps(ctx context.Context, serial string) ([]phonehost.App, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	out, err := adbCommand(ctx, serial, serverArgs("list_apps=true", "log_level=info", "video=false", "audio=false", "control=false")...).CombinedOutput()
	apps := parseAppList(string(out))
	if len(apps) == 0 && err != nil {
		return nil, fmt.Errorf("listing apps failed: %v: %s", err, lastLines(string(out), 3))
	}

	return apps, nil
}

// lastLines returns the last n non-empty lines of s, for an error message.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, " / ")
}

// session is one app on its own virtual display.
type session struct {
	id      uint32
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	port    int
	serial  string
	control net.Conn
	log     *io.PipeWriter // the server's output, read for its display's id

	pkg string
	// display is the id of the app's display (-1 until the server says) and
	// its size, as the server reported them.
	dmu          sync.Mutex
	display      int
	dispW, dispH int
	lastTree     []byte
	lastInput    atomic.Int64 // unix nanoseconds of the last input
	streaming    atomic.Bool

	cmu  sync.Mutex // one control message at a time
	once sync.Once
}

// noteLog reads the server's output for the line naming the app's display.
func (s *session) noteLog(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if w, h, id, ok := parseNewDisplay(sc.Text()); ok {
			s.dmu.Lock()
			s.display, s.dispW, s.dispH = id, w, h
			s.dmu.Unlock()
		}
	}
	io.Copy(io.Discard, r)
}

// sendTree sends the session its controls from dump, when they changed or
// have just caught up with the last input.
func (s *session) sendTree(a *agent, dump *xmlDisplays, started time.Time) {
	s.dmu.Lock()
	id, w, h := s.display, s.dispW, s.dispH
	s.dmu.Unlock()

	tree := treeFor(dump, id, s.pkg, w, h)
	if tree == nil {
		return
	}
	tree.Fresh = started.UnixNano() > s.lastInput.Load()

	raw, err := json.Marshal(tree)
	if err != nil {
		return
	}

	s.dmu.Lock()
	same := bytes.Equal(raw, s.lastTree)
	s.lastTree = raw
	s.dmu.Unlock()

	if same {
		return
	}

	a.send(phonehost.Message{Op: phonehost.OpTree, Session: s.id, Tree: tree})
}

// open starts the app of m on a new display and streams it until it ends.
func (a *agent) open(ctx context.Context, m phonehost.Message) {
	fail := func(err error) {
		a.cfg.Logf("could not open %s: %v", m.Package, err)
		a.send(phonehost.Message{Op: phonehost.OpClosed, Session: m.Session, Error: err.Error()})
	}

	w, h, dpi := m.Width, m.Height, m.DPI
	if w <= 0 || h <= 0 || w > 4096 || h > 4096 {
		w, h = 720, 1440
	}
	if dpi <= 0 || dpi > 1000 {
		dpi = 280
	}
	if !looksLikePackage(m.Package) {
		fail(fmt.Errorf("%q is not an app", m.Package))
		return
	}

	port, err := freePort()
	if err != nil {
		fail(err)
		return
	}

	scid := newSCID()
	sctx, cancel := context.WithCancel(ctx)
	s := &session{id: m.Session, cancel: cancel, port: port, serial: a.cfg.Serial, pkg: m.Package, display: -1, dispW: w, dispH: h}

	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()

	defer func() {
		s.stop()
		a.mu.Lock()
		delete(a.sessions, s.id)
		a.mu.Unlock()
	}()

	if out, err := adbCommand(sctx, a.cfg.Serial, "forward", "tcp:"+strconv.Itoa(port), "localabstract:scrcpy_"+scid).CombinedOutput(); err != nil {
		fail(fmt.Errorf("adb forward: %v %s", err, strings.TrimSpace(string(out))))
		return
	}

	s.cmd = adbCommand(sctx, a.cfg.Serial, serverArgs(
		"scid="+scid,
		"log_level=info",
		"tunnel_forward=true",
		"audio=false",
		"control=true",
		"video_codec=h264",
		fmt.Sprintf("new_display=%dx%d/%d", w, h, dpi),
		"max_fps=30",
		"video_bit_rate=4000000",
		"vd_destroy_content=true",
		"cleanup=true",
	)...)
	logs, logw := io.Pipe()
	s.log = logw
	s.cmd.Stdout, s.cmd.Stderr = logw, logw
	go s.noteLog(logs)
	if err := s.cmd.Start(); err != nil {
		fail(fmt.Errorf("starting the app server: %w", err))
		return
	}

	startCtx, startCancel := context.WithTimeout(sctx, 20*time.Second)
	video, vbr, err := dialForwarded(startCtx, port, true)
	if err == nil {
		defer video.Close()
		s.control, _, err = dialForwarded(startCtx, port, false)
	}
	startCancel()
	if err != nil {
		fail(err)
		return
	}

	// The device name, then the codec id, open the video socket.
	var meta [deviceNameLength + 4]byte
	video.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(vbr, meta[:]); err != nil {
		fail(fmt.Errorf("reading the stream's header: %w", err))
		return
	}
	video.SetReadDeadline(time.Time{})
	if codec := binary.BigEndian.Uint32(meta[deviceNameLength:]); codec != codecH264 {
		fail(fmt.Errorf("the phone sent codec %08x, not H.264", codec))
		return
	}

	// Device messages (clipboard) arrive on the control socket; nothing here
	// uses them, but they must be read or the server blocks writing them.
	go io.Copy(io.Discard, s.control)

	s.write(startAppMessage(m.Package))

	if err := a.send(phonehost.Message{Op: phonehost.OpOpened, Session: s.id, Codec: "h264"}); err != nil {
		return
	}

	a.cfg.Logf("opened %s", m.Package)

	s.streaming.Store(true)
	if a.sdk >= minTreeSDK {
		a.wakeTree()
	} else {
		a.send(phonehost.Message{Op: phonehost.OpTree, Session: s.id, Tree: &phonehost.Tree{
			W: w, H: h, Nodes: []phonehost.Node{},
			Error: "This phone runs an Android older than 11, which cannot show the app's buttons to the VM - only its picture.",
		}})
	}

	prefix := make([]byte, 4)
	binary.BigEndian.PutUint32(prefix, s.id)

	for {
		frame, err := readFrame(vbr)
		if err != nil {
			if sctx.Err() == nil {
				a.send(phonehost.Message{Op: phonehost.OpClosed, Session: s.id, Error: "the app's display closed"})
			} else {
				a.send(phonehost.Message{Op: phonehost.OpClosed, Session: s.id})
			}
			return
		}

		if err := a.conn.Write(wsconn.OpBinary, append(append([]byte(nil), prefix...), frame...)); err != nil {
			return
		}
	}
}

// input sends one viewer event to the app.
func (s *session) input(in phonehost.Input) {
	msgs, err := encodeInput(in)
	if err != nil {
		return
	}

	for _, msg := range msgs {
		s.write(msg)
	}
}

func (s *session) write(msg []byte) {
	s.cmu.Lock()
	defer s.cmu.Unlock()

	if s.control == nil {
		return
	}

	s.control.SetWriteDeadline(time.Now().Add(5 * time.Second))
	s.control.Write(msg)
}

// stop ends the server (which destroys the display and the app's windows on
// it) and removes the port forward.
func (s *session) stop() {
	s.once.Do(func() {
		s.cancel()

		s.cmu.Lock()
		if s.control != nil {
			s.control.Close()
		}
		s.cmu.Unlock()

		s.streaming.Store(false)
		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill()
			s.cmd.Wait()
		}
		if s.log != nil {
			s.log.Close()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		adbCommand(ctx, s.serial, "forward", "--remove", "tcp:"+strconv.Itoa(s.port)).Run()
	})
}

// PickSerial chooses the ADB device to use: the one given, else the phone's
// own `adb tcpip 5555` port (it survives Wi-Fi changes, until a restart),
// else the only device connected. It says what to do when there is none.
func PickSerial(ctx context.Context, want string) (string, error) {
	if _, err := exec.LookPath("adb"); err != nil {
		return "", errors.New("adb is not installed; in Termux run: pkg install android-tools")
	}

	out, err := exec.CommandContext(ctx, "adb", "devices").Output()
	if err != nil {
		return "", fmt.Errorf("adb devices: %w", err)
	}

	var ready []string
	for _, line := range strings.Split(string(out), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "device" {
			ready = append(ready, fields[0])
		}
	}

	if want != "" {
		for _, s := range ready {
			if s == want {
				return s, nil
			}
		}
		return "", fmt.Errorf("adb has no device %q connected (it has: %s)", want, strings.Join(ready, ", "))
	}

	for _, s := range ready {
		if s == "localhost:5555" || s == "127.0.0.1:5555" {
			return s, nil
		}
	}

	switch len(ready) {
	case 1:
		return ready[0], nil
	case 0:
		return "", errors.New(noDeviceHelp)
	}

	return "", fmt.Errorf("adb has several devices (%s); pick one with --phone-serial", strings.Join(ready, ", "))
}

// noDeviceHelp is what to do when ADB cannot reach the phone itself.
const noDeviceHelp = `adb is not connected to this phone. Once, in Termux:
  1. Settings > Developer options > Wireless debugging: on, then
     "Pair device with pairing code" (use split screen with Termux):
       adb pair localhost:<pairing port> <pairing code>
       adb connect localhost:<port shown on the Wireless debugging screen>
  2. So it keeps working when Wi-Fi changes (until the phone restarts):
       adb tcpip 5555
       adb connect localhost:5555`
