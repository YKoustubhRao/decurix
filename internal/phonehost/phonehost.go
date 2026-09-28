// Package phonehost is the control plane's side of a phone link: an Android
// phone running `decurix --phone` (internal/phoneagent, in Termux) keeps one
// WebSocket open to the control plane, and through it lists the apps
// installed on the phone and opens any of them on a virtual display of its
// own - so the app runs on the phone but is shown, and driven, in a VM.
//
// The phone dials out, never the other way round: it works over the free
// tunnel or the LAN, from mobile data or Wi-Fi, and nothing on the phone
// listens on the network. One connection carries everything - JSON messages
// both ways, and the video as binary messages from the phone, each prefixed
// by the session it belongs to.
//
// Depends on nothing internal but wsconn.
package phonehost

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// Message is one JSON message on a phone link, in either direction. Which
// fields are set depends on Op.
type Message struct {
	Op      string `json:"op"`
	Req     int    `json:"req,omitempty"`
	Session uint32 `json:"session,omitempty"`

	// hello (phone -> control plane)
	Model   string `json:"model,omitempty"`
	Android string `json:"android,omitempty"`

	// apps (the answer)
	Apps []App `json:"apps,omitempty"`

	// open (control plane -> phone)
	Package string `json:"package,omitempty"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	DPI     int    `json:"dpi,omitempty"`

	// opened (the answer)
	Codec string `json:"codec,omitempty"`

	// input (control plane -> phone)
	Input *Input `json:"input,omitempty"`

	// error, for a request or a session
	Error string `json:"error,omitempty"`
}

// Ops on a phone link.
const (
	OpHello  = "hello"  // phone: who it is
	OpApps   = "apps"   // control plane asks, phone answers with Apps
	OpOpen   = "open"   // control plane: start Package on a new display for Session
	OpOpened = "opened" // phone: Session is streaming, in Codec
	OpInput  = "input"  // control plane: Input for Session
	OpClose  = "close"  // control plane: end Session
	OpClosed = "closed" // phone: Session ended (Error says why, if it failed)
	OpError  = "error"  // phone: request Req failed
)

// App is one app installed on a phone.
type App struct {
	Package string `json:"package"`
	Name    string `json:"name"`
	System  bool   `json:"system,omitempty"`
}

// Input is one event from a viewer, in the video's own pixels.
type Input struct {
	// Kind is touch, scroll, key, text or back.
	Kind string `json:"kind"`
	// Action: 0 down, 1 up, 2 move (touch and key).
	Action int `json:"action,omitempty"`
	X      int `json:"x,omitempty"`
	Y      int `json:"y,omitempty"`
	// W and H are the size of the picture the position is in; the phone
	// ignores an event for a size its display no longer has.
	W       int     `json:"w,omitempty"`
	H       int     `json:"h,omitempty"`
	Key     int     `json:"key,omitempty"`      // an Android KEYCODE_*
	Meta    int     `json:"meta,omitempty"`     // Android META_* state
	Text    string  `json:"text,omitempty"`     // text to type
	ScrollX float64 `json:"scroll_x,omitempty"` // -16..16
	ScrollY float64 `json:"scroll_y,omitempty"`
}

// Info is a phone as the dashboard lists it.
type Info struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Model     string    `json:"model,omitempty"`
	Android   string    `json:"android,omitempty"`
	Connected time.Time `json:"connected"`
	Apps      int       `json:"apps"`
}

const (
	// pingEvery keeps the link alive through the tunnel, which drops a quiet
	// connection, and notices a phone that went away.
	pingEvery = 25 * time.Second
	// readIdle is how long the link may be silent before it is taken as dead;
	// the phone answers every ping.
	readIdle = 75 * time.Second
	// appsFresh is how long a phone's app list is reused.
	appsFresh = 2 * time.Minute
	// frameBacklog bounds the video messages waiting for one viewer. A viewer
	// that falls this far behind is closed rather than shown a broken picture;
	// it opens again and starts from a key frame.
	frameBacklog = 512
)

// ErrNoPhone is a phone that is not connected right now.
var ErrNoPhone = errors.New("phonehost: that phone is not connected")

// Hub is every phone connected right now.
type Hub struct {
	mu     sync.Mutex
	phones map[string]*phone
	nextID uint32
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{phones: make(map[string]*phone)}
}

type phone struct {
	hub  *Hub
	id   string
	name string
	conn *wsconn.Conn

	mu        sync.Mutex
	model     string
	android   string
	connected time.Time
	apps      []App
	appsTime  time.Time
	nextReq   int
	pending   map[int]chan Message
	sessions  map[uint32]*Session
	closed    bool
}

// Serve runs the link of the phone paired as deviceID until it drops. A
// phone that connects again replaces its older link.
func (h *Hub) Serve(ctx context.Context, deviceID, name string, conn *wsconn.Conn) {
	p := &phone{
		hub:       h,
		id:        deviceID,
		name:      name,
		conn:      conn,
		connected: time.Now(),
		pending:   make(map[int]chan Message),
		sessions:  make(map[uint32]*Session),
	}

	h.mu.Lock()
	old := h.phones[deviceID]
	h.phones[deviceID] = p
	h.mu.Unlock()

	if old != nil {
		old.shutdown("the phone connected again")
	}

	log.Printf("phonehost: %s connected", name)

	done := make(chan struct{})
	go p.keepAlive(done)
	p.readLoop()
	close(done)

	h.mu.Lock()
	if h.phones[deviceID] == p {
		delete(h.phones, deviceID)
	}
	h.mu.Unlock()

	p.shutdown("the phone disconnected")
	log.Printf("phonehost: %s disconnected", name)
}

func (p *phone) keepAlive(done chan struct{}) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()

	for {
		select {
		case <-done:
			return
		case <-t.C:
			if p.conn.Write(wsconn.OpPing, nil) != nil {
				p.conn.Close()
				return
			}
		}
	}
}

func (p *phone) readLoop() {
	for {
		op, msg, err := p.conn.Read(readIdle)
		if err != nil {
			return
		}

		if op == wsconn.OpBinary {
			p.deliverFrame(msg)
			continue
		}

		var m Message
		if json.Unmarshal(msg, &m) != nil {
			continue
		}

		p.handle(m)
	}
}

// handle takes one JSON message from the phone.
func (p *phone) handle(m Message) {
	switch m.Op {
	case OpHello:
		p.mu.Lock()
		p.model, p.android = m.Model, m.Android
		p.mu.Unlock()
		return
	case OpOpened, OpClosed:
		p.mu.Lock()
		s := p.sessions[m.Session]
		p.mu.Unlock()
		if s == nil {
			return
		}
		if m.Op == OpOpened {
			s.opened(m.Codec)
		} else {
			s.end(m.Error)
		}
		return
	}

	if m.Req == 0 {
		return
	}

	p.mu.Lock()
	ch := p.pending[m.Req]
	delete(p.pending, m.Req)
	p.mu.Unlock()

	if ch != nil {
		ch <- m
	}
}

// deliverFrame hands one video message ([session u32][frame]) to its viewer.
func (p *phone) deliverFrame(msg []byte) {
	if len(msg) < 4 {
		return
	}

	id := binary.BigEndian.Uint32(msg)

	p.mu.Lock()
	s := p.sessions[id]
	p.mu.Unlock()

	if s == nil {
		return
	}

	select {
	case s.frames <- msg[4:]:
	default:
		s.end("the viewer fell too far behind the phone")
		p.send(Message{Op: OpClose, Session: id})
	}
}

// send writes one message to the phone.
func (p *phone) send(m Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return p.conn.Write(wsconn.OpText, raw)
}

// request sends m and waits for the phone's answer to it.
func (p *phone) request(ctx context.Context, m Message) (Message, error) {
	ch := make(chan Message, 1)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return Message{}, ErrNoPhone
	}
	p.nextReq++
	m.Req = p.nextReq
	p.pending[m.Req] = ch
	p.mu.Unlock()

	if err := p.send(m); err != nil {
		return Message{}, err
	}

	select {
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, m.Req)
		p.mu.Unlock()
		return Message{}, ctx.Err()
	case ans, ok := <-ch:
		if !ok {
			return Message{}, ErrNoPhone
		}
		if ans.Op == OpError {
			return Message{}, errors.New(ans.Error)
		}
		return ans, nil
	}
}

// shutdown ends the link: every waiting request fails and every session ends.
func (p *phone) shutdown(reason string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	pending := p.pending
	p.pending = map[int]chan Message{}
	sessions := p.sessions
	p.sessions = map[uint32]*Session{}
	p.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	for _, s := range sessions {
		s.end(reason)
	}

	p.conn.Close()
}

func (h *Hub) phone(id string) *phone {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.phones[id]
}

// Phones lists the phones connected right now, by name.
func (h *Hub) Phones() []Info {
	h.mu.Lock()
	list := make([]*phone, 0, len(h.phones))
	for _, p := range h.phones {
		list = append(list, p)
	}
	h.mu.Unlock()

	out := make([]Info, 0, len(list))
	for _, p := range list {
		p.mu.Lock()
		out = append(out, Info{ID: p.id, Name: p.name, Model: p.model, Android: p.android, Connected: p.connected, Apps: len(p.apps)})
		p.mu.Unlock()
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

// Name returns the name of the phone paired as id, and whether it is connected.
func (h *Hub) Name(id string) (string, bool) {
	p := h.phone(id)
	if p == nil {
		return "", false
	}

	return p.name, true
}

// Apps returns the apps installed on the phone paired as id, asking the
// phone when its last answer is older than appsFresh (or refresh is set).
func (h *Hub) Apps(ctx context.Context, id string, refresh bool) ([]App, error) {
	p := h.phone(id)
	if p == nil {
		return nil, ErrNoPhone
	}

	p.mu.Lock()
	if !refresh && p.apps != nil && time.Since(p.appsTime) < appsFresh {
		apps := append([]App(nil), p.apps...)
		p.mu.Unlock()
		return apps, nil
	}
	p.mu.Unlock()

	ans, err := p.request(ctx, Message{Op: OpApps})
	if err != nil {
		return nil, fmt.Errorf("asking %s for its apps: %w", p.name, err)
	}

	p.mu.Lock()
	p.apps, p.appsTime = ans.Apps, time.Now()
	p.mu.Unlock()

	return append([]App(nil), ans.Apps...), nil
}

// Cached returns the apps the phone paired as id last reported, without
// asking it again (nil when it has not been asked, or is not connected).
func (h *Hub) Cached(id string) []App {
	p := h.phone(id)
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]App(nil), p.apps...)
}

// Session is one app open on a phone's virtual display, shown to one viewer.
type Session struct {
	ID    uint32
	phone *phone

	frames chan []byte
	ready  chan struct{}
	done   chan struct{}

	mu    sync.Mutex
	codec string
	err   string
	ended bool
}

// Open starts pkg on a new w x h display (dpi) of the phone paired as id and
// waits, up to ctx, until it is streaming.
func (h *Hub) Open(ctx context.Context, id, pkg string, w, hgt, dpi int) (*Session, error) {
	p := h.phone(id)
	if p == nil {
		return nil, ErrNoPhone
	}

	h.mu.Lock()
	h.nextID++
	sid := h.nextID
	h.mu.Unlock()

	s := &Session{
		ID:     sid,
		phone:  p,
		frames: make(chan []byte, frameBacklog),
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrNoPhone
	}
	p.sessions[sid] = s
	p.mu.Unlock()

	if err := p.send(Message{Op: OpOpen, Session: sid, Package: pkg, Width: w, Height: hgt, DPI: dpi}); err != nil {
		s.Close()
		return nil, err
	}

	select {
	case <-s.ready:
		return s, nil
	case <-s.done:
		return nil, errors.New(s.Err())
	case <-ctx.Done():
		s.Close()
		return nil, fmt.Errorf("%s did not start %s in time", p.name, pkg)
	}
}

func (s *Session) opened(codec string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ended || s.codec != "" {
		return
	}

	s.codec = codec
	close(s.ready)
}

// end finishes the session once, with reason as its error ("" for a clean end).
func (s *Session) end(reason string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.err = reason
	s.mu.Unlock()

	close(s.done)

	s.phone.mu.Lock()
	delete(s.phone.sessions, s.ID)
	s.phone.mu.Unlock()
}

// Codec is the video's codec ("h264").
func (s *Session) Codec() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.codec
}

// Frames delivers the video: each message a 12-byte scrcpy frame header and
// its payload (a session packet has none).
func (s *Session) Frames() <-chan []byte { return s.frames }

// Done is closed when the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err says why the session ended, or "" if it ended cleanly or has not.
func (s *Session) Err() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err == "" && s.ended {
		return "the app was closed"
	}

	return s.err
}

// Input sends one event to the app.
func (s *Session) Input(in Input) error {
	return s.phone.send(Message{Op: OpInput, Session: s.ID, Input: &in})
}

// Close ends the session and tells the phone to close the app's display.
func (s *Session) Close() {
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()

	if !ended {
		_ = s.phone.send(Message{Op: OpClose, Session: s.ID})
	}

	s.end("")
}
