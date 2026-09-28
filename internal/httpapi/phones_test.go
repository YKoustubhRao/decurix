package httpapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/devices"
	"github.com/YKoustubhRao/decurix/internal/phonehost"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/winhost"
	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// phoneServer is a Server with phone links on, behind both muxes' routes, and a paired phone's token.
func phoneServer(t *testing.T) (*Server, *httptest.Server, string, devices.Device) {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := devices.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := store.Pair("Pixel 8")
	dev, _ := store.Lookup(token)

	s := &Server{Web: webui.NewRenderer(), Devices: store}
	s.SetPhones(phonehost.NewHub())
	t.Cleanup(func() { desktop.SetExtraHostApps(nil) })

	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	s.RegisterLocalRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return s, srv, token, dev
}

// linkFakePhone connects a phone that answers the app list and opens apps by sending one frame.
func linkFakePhone(t *testing.T, srv *httptest.Server, token string) *wsconn.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := wsconn.Dial(ctx, srv.URL+"/api/phones/connect", http.Header{"Cookie": {phoneDeviceCookie + "=" + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	send := func(m phonehost.Message) {
		raw, _ := json.Marshal(m)
		conn.Write(wsconn.OpText, raw)
	}
	send(phonehost.Message{Op: phonehost.OpHello, Model: "Pixel 8"})

	go func() {
		for {
			op, raw, err := conn.Read(10 * time.Second)
			if err != nil {
				return
			}
			if op != wsconn.OpText {
				continue
			}
			var m phonehost.Message
			json.Unmarshal(raw, &m)
			switch m.Op {
			case phonehost.OpApps:
				send(phonehost.Message{Op: phonehost.OpApps, Req: m.Req, Apps: []phonehost.App{{Package: "com.whatsapp", Name: "WhatsApp"}}})
			case phonehost.OpOpen:
				send(phonehost.Message{Op: phonehost.OpOpened, Session: m.Session, Codec: "h264"})
				frame := make([]byte, 4+12)
				binary.BigEndian.PutUint32(frame, m.Session)
				frame[4] = 0x80 // a session packet
				conn.Write(wsconn.OpBinary, frame)
				send(phonehost.Message{Op: phonehost.OpTree, Session: m.Session, Tree: &phonehost.Tree{
					W: 720, H: 1440, Fresh: true, Nodes: []phonehost.Node{{Role: "button", Name: "Send", Box: [4]int{620, 1360, 80, 80}}},
				}})
			}
		}
	}()

	return conn
}

func TestAPhoneWithoutAPairingCannotLink(t *testing.T) {
	_, srv, _, _ := phoneServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := wsconn.Dial(ctx, srv.URL+"/api/phones/connect", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("no cookie: %v", err)
	}
	if _, err := wsconn.Dial(ctx, srv.URL+"/api/phones/connect", http.Header{"Cookie": {phoneDeviceCookie + "=forged"}}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a forged token: %v", err)
	}
}

func TestALinkedPhonesAppsAreListedAndOpenInAViewer(t *testing.T) {
	s, srv, token, dev := phoneServer(t)
	linkFakePhone(t, srv, token)

	deadline := time.Now().Add(3 * time.Second)
	for len(s.Phones.Phones()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the phone never linked")
		}
		time.Sleep(10 * time.Millisecond)
	}

	s.refreshPhoneApps(context.Background(), true)

	items := s.phoneAppItems()
	if len(items) != 1 || items[0].Mode != desktop.ModePhone || items[0].Name != "WhatsApp" ||
		items[0].HostCmd != desktop.PhoneCmd(dev.ID, "com.whatsapp") || !strings.Contains(items[0].Category, "Pixel 8") {
		t.Fatalf("app list entries = %+v", items)
	}

	// A viewer with no token, or one signed for another app, is refused.
	id := "vm1-phone-whatsapp"
	good := winhost.Token(s.hostGUISecretKey(), id, phoneTargetPrefix+dev.ID, "com.whatsapp")
	q := url.Values{"id": {id}, "device": {dev.ID}, "package": {"com.whatsapp"}, "token": {good}}

	for _, bad := range []url.Values{
		{"id": {id}, "device": {dev.ID}, "package": {"com.whatsapp"}},
		{"id": {id}, "device": {dev.ID}, "package": {"com.bank"}, "token": {good}},
	} {
		resp, err := http.Get(srv.URL + "/phone-app/view?" + bad.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%v: status %d, want 403", bad, resp.StatusCode)
		}
	}

	resp, err := http.Get(srv.URL + "/phone-app/view?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the viewer page: %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	viewer, err := wsconn.Dial(ctx, srv.URL+"/api/phone-app/ws?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()

	op, raw, err := viewer.Read(5 * time.Second)
	if err != nil || op != wsconn.OpText || !strings.Contains(string(raw), `"opened":true`) {
		t.Fatalf("first message %d %s %v", op, raw, err)
	}

	op, raw, err = viewer.Read(5 * time.Second)
	if err != nil || op != wsconn.OpBinary || len(raw) != 12 || raw[0] != 0x80 {
		t.Fatalf("the phone's frame reached the viewer as %d %v %v (want the 12-byte packet, no session prefix)", op, raw, err)
	}

	// The app's controls follow, as the phone read them.
	op, raw, err = viewer.Read(5 * time.Second)
	var got struct{ Tree *phonehost.Tree }
	if err != nil || op != wsconn.OpText || json.Unmarshal(raw, &got) != nil || got.Tree == nil ||
		len(got.Tree.Nodes) != 1 || got.Tree.Nodes[0].Name != "Send" || !got.Tree.Fresh {
		t.Fatalf("the app's controls reached the viewer as %d %s %v", op, raw, err)
	}
}

func TestAViewerForAnUnlinkedPhoneSaysHowToLinkIt(t *testing.T) {
	s, srv, _, dev := phoneServer(t)

	id := "vm1-phone-whatsapp"
	q := url.Values{"id": {id}, "device": {dev.ID}, "package": {"com.whatsapp"},
		"token": {winhost.Token(s.hostGUISecretKey(), id, phoneTargetPrefix+dev.ID, "com.whatsapp")}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	viewer, err := wsconn.Dial(ctx, srv.URL+"/api/phone-app/ws?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()

	_, raw, err := viewer.Read(5 * time.Second)
	if err != nil || !strings.Contains(string(raw), "decurix --phone") {
		t.Fatalf("got %s %v", raw, err)
	}
}
