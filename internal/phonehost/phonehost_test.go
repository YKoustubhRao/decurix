package phonehost

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// fakePhone is the other end of a link, as a test drives it.
type fakePhone struct {
	t    *testing.T
	conn *wsconn.Conn
}

func (f *fakePhone) send(m Message) {
	raw, _ := json.Marshal(m)
	if err := f.conn.Write(wsconn.OpText, raw); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakePhone) recv() Message {
	f.t.Helper()
	for {
		op, raw, err := f.conn.Read(5 * time.Second)
		if err != nil {
			f.t.Fatal(err)
		}
		if op != wsconn.OpText {
			continue
		}
		var m Message
		json.Unmarshal(raw, &m)
		return m
	}
}

// link starts a hub behind a test server and connects a fake phone as "dev1".
func link(t *testing.T) (*Hub, *fakePhone) {
	t.Helper()

	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wsconn.Upgrade(w, r)
		if err != nil {
			return
		}
		hub.Serve(r.Context(), "dev1", "Pixel", c)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := wsconn.Dial(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	f := &fakePhone{t: t, conn: conn}
	f.send(Message{Op: OpHello, Model: "Pixel 8", Android: "15"})

	deadline := time.Now().Add(3 * time.Second)
	for len(hub.Phones()) == 0 || hub.Phones()[0].Model == "" {
		if time.Now().After(deadline) {
			t.Fatal("the phone never showed up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	return hub, f
}

func TestAppsAreAskedForAndKept(t *testing.T) {
	hub, phone := link(t)

	go func() {
		m := phone.recv()
		if m.Op != OpApps {
			t.Errorf("asked %q", m.Op)
		}
		phone.send(Message{Op: OpApps, Req: m.Req, Apps: []App{{Package: "com.whatsapp", Name: "WhatsApp"}}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	apps, err := hub.Apps(ctx, "dev1", false)
	if err != nil || len(apps) != 1 || apps[0].Name != "WhatsApp" {
		t.Fatalf("Apps = %v, %v", apps, err)
	}

	// Asked again soon after: answered from what the phone said, no second request.
	if apps, err := hub.Apps(ctx, "dev1", false); err != nil || len(apps) != 1 {
		t.Fatalf("cached Apps = %v, %v", apps, err)
	}

	if info := hub.Phones(); len(info) != 1 || info[0].Model != "Pixel 8" || info[0].Apps != 1 {
		t.Errorf("Phones = %+v", info)
	}

	if _, err := hub.Apps(ctx, "nobody", false); err != ErrNoPhone {
		t.Errorf("an unknown phone: %v", err)
	}
}

func TestASessionOpensStreamsTakesInputAndCloses(t *testing.T) {
	hub, phone := link(t)

	opened := make(chan Message, 1)
	go func() {
		m := phone.recv()
		opened <- m
		phone.send(Message{Op: OpOpened, Session: m.Session, Codec: "h264"})

		frame := make([]byte, 4+12+3)
		binary.BigEndian.PutUint32(frame, m.Session)
		frame[4+11] = 3
		copy(frame[16:], []byte{1, 2, 3})
		phone.conn.Write(wsconn.OpBinary, frame)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := hub.Open(ctx, "dev1", "com.whatsapp", 720, 1440, 280)
	if err != nil {
		t.Fatal(err)
	}
	if m := <-opened; m.Op != OpOpen || m.Package != "com.whatsapp" || m.Width != 720 || m.DPI != 280 {
		t.Fatalf("the phone was asked %+v", m)
	}
	if s.Codec() != "h264" {
		t.Errorf("codec %q", s.Codec())
	}

	select {
	case f := <-s.Frames():
		if len(f) != 15 || f[12] != 1 {
			t.Fatalf("frame %v: the session prefix should be gone, the header and payload kept", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame arrived")
	}

	if err := s.Input(Input{Kind: "touch", X: 5, Y: 6, W: 720, H: 1440}); err != nil {
		t.Fatal(err)
	}
	if m := phone.recv(); m.Op != OpInput || m.Input == nil || m.Input.X != 5 || m.Session != s.ID {
		t.Fatalf("input arrived as %+v", m)
	}

	s.Close()
	if m := phone.recv(); m.Op != OpClose || m.Session != s.ID {
		t.Fatalf("close arrived as %+v", m)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("a closed session is not done")
	}
}

func TestTheAppsControlsReachItsSessionNewestOnly(t *testing.T) {
	hub, phone := link(t)

	go func() {
		m := phone.recv()
		phone.send(Message{Op: OpOpened, Session: m.Session, Codec: "h264"})
		// Two trees before anyone reads: only the second is still of use.
		phone.send(Message{Op: OpTree, Session: m.Session, Tree: &Tree{W: 720, H: 1440, Nodes: []Node{{Role: "button", Name: "Pay"}}}})
		phone.send(Message{Op: OpTree, Session: m.Session, Tree: &Tree{W: 720, H: 1440, Fresh: true, Nodes: []Node{{Role: "button", Name: "Send"}}}})
		// One for a session nobody has is dropped.
		phone.send(Message{Op: OpTree, Session: m.Session + 100, Tree: &Tree{}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := hub.Open(ctx, "dev1", "com.whatsapp", 720, 1440, 280)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case tr := <-s.Trees():
			if tr.Nodes[0].Name == "Send" {
				if !tr.Fresh || tr.W != 720 {
					t.Errorf("tree = %+v", tr)
				}
				return
			}
			// The first may be taken before the second arrives; then the second follows.
		case <-deadline:
			t.Fatal("the newest tree never arrived")
		}
	}
}

func TestAPhoneThatCannotOpenSaysWhy(t *testing.T) {
	hub, phone := link(t)

	go func() {
		m := phone.recv()
		phone.send(Message{Op: OpClosed, Session: m.Session, Error: "no such app"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := hub.Open(ctx, "dev1", "com.nothing", 720, 1440, 280); err == nil || !strings.Contains(err.Error(), "no such app") {
		t.Fatalf("err = %v", err)
	}
}

func TestADroppedPhoneEndsItsSessions(t *testing.T) {
	hub, phone := link(t)

	go func() {
		m := phone.recv()
		phone.send(Message{Op: OpOpened, Session: m.Session, Codec: "h264"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := hub.Open(ctx, "dev1", "com.whatsapp", 720, 1440, 280)
	if err != nil {
		t.Fatal(err)
	}

	phone.conn.Close()

	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived its phone")
	}
	if s.Err() == "" {
		t.Error("no reason given")
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(hub.Phones()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the dropped phone is still listed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
