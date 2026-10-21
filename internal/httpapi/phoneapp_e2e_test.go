//go:build e2e

package httpapi

// A phone app in a real desktop, driven by the desktop's own agent code:
//
//	go test -tags e2e -run TestAPhoneAppIsButtonsInARealDesktop -v ./internal/httpapi/
//
// Needs Docker and decurix-desktop:basic (about a minute). The control plane
// is this package's real handlers; the phone is a fake speaking the real
// protocol, streaming a real H.264 picture (testdata/phone-frame.h264, one
// 360x760 frame from libx264) and the app's controls as a phone reads them.
// In the desktop, the viewer page runs in its own Chromium, and this tree's
// agent (copied in) reads it with computer/cdp.observe, judges it with the
// approval gate, clicks it and types into it. What the phone receives is
// checked here.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/devices"
	"github.com/YKoustubhRao/decurix/internal/phonehost"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/winhost"
	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// scrcpyPacket frames payload as the phone sends video: a 12-byte header
// (flags in the top bits of the first byte, size last) and the payload.
func scrcpyPacket(session uint32, flags byte, payload []byte) []byte {
	b := make([]byte, 4+12+len(payload))
	binary.BigEndian.PutUint32(b, session)
	b[4] = flags
	binary.BigEndian.PutUint32(b[4+8:], uint32(len(payload)))
	copy(b[16:], payload)
	return b
}

// fakeScreen is the phone's side of the app: a message field and a Send button.
type fakeScreen struct {
	mu        sync.Mutex
	message   string
	selectAll bool
	inputs    []phonehost.Input
	at        []time.Time
}

func (f *fakeScreen) take(in phonehost.Input) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inputs = append(f.inputs, in)
	f.at = append(f.at, time.Now())
	switch {
	case in.Kind == "text":
		f.message += in.Text
	case in.Kind == "key" && in.Action == 0 && in.Key == 29 && in.Meta&0x1000 != 0:
		f.selectAll = true
	case in.Kind == "key" && in.Action == 0 && in.Key == 67:
		if f.selectAll {
			f.message = ""
		} else if f.message != "" {
			f.message = f.message[:len(f.message)-1]
		}
		f.selectAll = false
	}
}

func (f *fakeScreen) tree() *phonehost.Tree {
	f.mu.Lock()
	defer f.mu.Unlock()

	return &phonehost.Tree{W: 360, H: 760, Fresh: true, Nodes: []phonehost.Node{
		{Role: "text", Name: "Mum", Box: [4]int{20, 20, 200, 50}},
		{Role: "textbox", Name: "Message", Value: f.message, Box: [4]int{10, 690, 280, 50}},
		{Role: "button", Name: "Send", Box: [4]int{300, 690, 50, 50}},
	}}
}

func TestAPhoneAppIsButtonsInARealDesktop(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	if exec.Command("docker", "image", "inspect", "decurix-desktop:basic").Run() != nil {
		t.Skip("no decurix-desktop:basic image")
	}

	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..")

	h264, err := os.ReadFile(filepath.Join(filepath.Dir(here), "testdata", "phone-frame.h264"))
	if err != nil {
		t.Fatal(err)
	}
	// SPS and PPS travel as the config packet, the picture as a key frame (SEI dropped).
	sei := bytes.Index(h264, []byte{0, 0, 1, 6})
	idr := bytes.Index(h264, []byte{0, 0, 1, 0x65})
	if sei < 0 || idr < 0 {
		t.Fatal("the fixture is not the SPS, PPS, SEI, IDR stream it should be")
	}
	config, key := h264[:sei], h264[idr:]

	// The control plane, reachable from a container.
	db := testDB(t)
	store, _ := devices.NewStore(db)
	token, _ := store.Pair("Pixel 8")
	dev, _ := store.Lookup(token)

	s := &Server{Web: webui.NewRenderer(), Devices: store}
	s.SetPhones(phonehost.NewHub())
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	s.RegisterLocalRoutes(mux)
	srv := httptest.NewUnstartedServer(mux)
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	port := l.Addr().(*net.TCPAddr).Port

	// The phone.
	screen := &fakeScreen{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := wsconn.Dial(ctx, srv.URL+"/api/phones/connect", http.Header{"Cookie": {phoneDeviceCookie + "=" + token}})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var wmu sync.Mutex
	write := func(op byte, b []byte) { wmu.Lock(); conn.Write(op, b); wmu.Unlock() }
	send := func(m phonehost.Message) { raw, _ := json.Marshal(m); write(wsconn.OpText, raw) }
	send(phonehost.Message{Op: phonehost.OpHello, Model: "Pixel 8", Android: "15"})

	go func() {
		for {
			op, raw, err := conn.Read(5 * time.Minute)
			if err != nil {
				return
			}
			if op != wsconn.OpText {
				continue
			}
			var m phonehost.Message
			json.Unmarshal(raw, &m)
			switch m.Op {
			case phonehost.OpOpen:
				send(phonehost.Message{Op: phonehost.OpOpened, Session: m.Session, Codec: "h264"})
				sp := make([]byte, 4+12)
				binary.BigEndian.PutUint32(sp, m.Session)
				sp[4] = 0x80
				binary.BigEndian.PutUint32(sp[8:], 360)
				binary.BigEndian.PutUint32(sp[12:], 760)
				write(wsconn.OpBinary, sp)
				write(wsconn.OpBinary, scrcpyPacket(m.Session, 0x40, config))
				go func(id uint32) {
					for i := 0; i < 20; i++ {
						write(wsconn.OpBinary, scrcpyPacket(id, 0x20, key))
						time.Sleep(500 * time.Millisecond)
					}
				}(m.Session)
				send(phonehost.Message{Op: phonehost.OpTree, Session: m.Session, Tree: screen.tree()})
			case phonehost.OpInput:
				if m.Input != nil {
					screen.take(*m.Input)
					// A phone reads its controls again after input, and says they are fresh.
					go func(id uint32) {
						time.Sleep(200 * time.Millisecond)
						send(phonehost.Message{Op: phonehost.OpTree, Session: id, Tree: screen.tree()})
					}(m.Session)
				}
			}
		}
	}()

	waitFor(t, 3*time.Second, "the phone to link", func() bool { return len(s.Phones.Phones()) > 0 })

	id := "e2e-phone-whatsapp"
	q := url.Values{
		"id": {id}, "desktop": {"e2e"}, "title": {"WhatsApp"}, "device": {dev.ID}, "package": {"com.whatsapp"},
		"token": {winhost.Token(s.hostGUISecretKey(), id, phoneTargetPrefix+dev.ID, "com.whatsapp")},
	}
	view := fmt.Sprintf("http://host.docker.internal:%d/phone-app/view?%s", port, q.Encode())

	// The desktop. Its agent has no model to reach, on purpose: nothing here may touch a shared Ollama.
	name := fmt.Sprintf("decurix-e2e-phone-%d", time.Now().Unix())
	run := exec.Command("docker", "run", "-d", "--name", name, "--shm-size=512m",
		"--add-host", "host.docker.internal:host-gateway",
		"-e", "VNC_PW=e2e", "-e", "AGENT_HARNESS=ollama", "-e", "OLLAMA_URL=http://127.0.0.1:9",
		"decurix-desktop:basic")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	if os.Getenv("E2E_KEEP") == "" {
		defer exec.Command("docker", "rm", "-f", name).Run()
	} else {
		t.Logf("keeping %s; the viewer is %s", name, view)
	}

	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}

	// The browser's launch flags (the DevTools port) are installed by the desktop's session on start.
	deadline = time.Now().Add(90 * time.Second)
	for {
		if _, err := docker("exec", name, "test", "-f", "/home/headless/.chromium-browser.init"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the desktop's session never started")
		}
		time.Sleep(time.Second)
	}

	// The phone's picture is H.264, which the desktop's browser (Google Chrome, agent/install-browser.sh) decodes.
	if out, err := docker("exec", name, "/opt/google/chrome/google-chrome", "--version"); err != nil {
		t.Fatalf("the desktop image has no Google Chrome - rebuild it (decurix --build-images): %v %s", err, out)
	}

	if out, err := docker("cp", filepath.Join(repo, "agent", "decurix_agent"), name+":/tmp/decurix_agent"); err != nil {
		t.Fatalf("docker cp: %v %s", err, out)
	}
	if out, err := docker("exec", "-d", "-u", "headless", "-e", "DISPLAY=:1", name,
		"chromium-browser", "--app="+view, "--window-size=420,860"); err != nil {
		t.Fatalf("starting Chromium: %v %s", err, out)
	}

	script := `
import json, sys, time
sys.path.insert(0, "/tmp")
from decurix_agent.computer import approval, cdp

def find(name, role):
    deadline = time.time() + 60
    last = None
    while time.time() < deadline:
        try:
            elements, group = cdp.observe()
            last = (group.get("window_title"), [[e["role"], e["name"]] for e in elements])
            for e in elements:
                if e["name"] == name and e["role"] == role:
                    return e, elements, group
        except Exception as exc:
            last = repr(exc)
        time.sleep(1)
    raise SystemExit("never saw %s %s; last look: %r" % (role, name, last))

send, elements, group = find("Send", "button")
page = cdp.active_page()
picture = page.evaluate("""(function () {
  var v = document.getElementById('video'), src = v.hidden ? document.getElementById('screen') : v;
  var c = document.createElement('canvas'); c.width = 360; c.height = 760;
  var g = c.getContext('2d'); g.drawImage(src, 0, 0, 360, 760);
  var d = g.getImageData(325, 715, 1, 1).data;
  return (v.hidden ? 'canvas ' : 'video ') + d[0] + ',' + d[1] + ',' + d[2];
})()""")
page.close()
out = {
    "picture": picture,
    "elements": [[e["role"], e["name"], e["box"]] for e in elements],
    "window": group["window_title"],
    "send_gated": approval.assess("click", send),
    "enter_gated": approval.assess("key", None, keys=["Enter"], context={"window": group["window_title"]}),
}

field, _, _ = find("Message", "textbox")
cdp.type_text(field, "hi there")
time.sleep(2)
field, _, _ = find("Message", "textbox")
out["value_after_typing"] = field["value"]
cdp.type_text(field, "bye", replace=True)
time.sleep(2)

# An engine's click goes through core.act: the gate stops it before anything
# reaches the phone, and a person's yes lets exactly that click through.
from decurix_agent.computer import core
approval.reset()
first = core.act("click", role="button", name="Send")
out["act_first"] = first["status"]
time.sleep(2)
out["granted_at"] = time.time()
out["reply"] = approval.resolve_reply("yes")
second = core.act("click", role="button", name="Send")
out["act_second"] = second["status"]
time.sleep(1)
print("RESULT " + json.dumps(out))
`
	cmd := exec.Command("docker", "exec", "-i", "-u", "headless", "-e", "DISPLAY=:1", name, "python3", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the agent's side failed: %v\n%s", err, out)
	}

	var result struct {
		Picture          string  `json:"picture"`
		Elements         [][]any `json:"elements"`
		Window           string  `json:"window"`
		SendGated        *string `json:"send_gated"`
		EnterGated       *string `json:"enter_gated"`
		ValueAfterTyping string  `json:"value_after_typing"`
		ActFirst         string  `json:"act_first"`
		ActSecond        string  `json:"act_second"`
		Reply            string  `json:"reply"`
		GrantedAt        float64 `json:"granted_at"`
	}
	i := strings.LastIndex(string(out), "RESULT ")
	if i < 0 || json.Unmarshal([]byte(strings.TrimSpace(string(out)[i+7:])), &result) != nil {
		t.Fatalf("no result:\n%s", out)
	}
	t.Logf("the desktop saw %s: %v", result.Window, result.Elements)

	// The picture is the phone's: under Send the fixture is green (0x25d366).
	var surface string
	var r, g, b int
	if n, _ := fmt.Sscanf(result.Picture, "%s %d,%d,%d", &surface, &r, &g, &b); n != 4 || g < 150 || r > 90 {
		t.Errorf("the phone's picture is not on screen: %q", result.Picture)
	} else {
		t.Logf("picture on the %s: %d,%d,%d under Send", surface, r, g, b)
	}

	if result.SendGated == nil {
		t.Error("the approval gate let a phone app's Send through")
	} else {
		t.Logf("Send: %s", *result.SendGated)
	}
	if result.EnterGated == nil {
		t.Error("Enter in a phone messaging app was not stopped")
	}
	if result.ValueAfterTyping != "hi there" {
		t.Errorf("after typing, the field read %q", result.ValueAfterTyping)
	}

	screen.mu.Lock()
	defer screen.mu.Unlock()

	if screen.message != "bye" {
		t.Errorf("the phone's field holds %q, want %q (typing, then replacing it)", screen.message, "bye")
	}

	if result.ActFirst != "needs_approval" || result.Reply != "granted" || result.ActSecond != "done" {
		t.Errorf("an engine's click on Send: first %q, reply %q, then %q (want needs_approval, granted, done)",
			result.ActFirst, result.Reply, result.ActSecond)
	}

	// Nothing touched Send before the person's yes; after it, one tap in its
	// middle (325, 715 in the video's pixels) did.
	granted := time.Unix(0, int64(result.GrantedAt*1e9))
	var before, after int
	for i, in := range screen.inputs {
		if in.Kind == "touch" && in.Action == 1 && abs(in.X-325) <= 4 && abs(in.Y-715) <= 4 && in.W == 360 && in.H == 760 {
			if screen.at[i].Before(granted) {
				before++
			} else {
				after++
			}
		}
	}
	if before != 0 || after != 1 {
		t.Errorf("taps on Send: %d before the yes, %d after (want 0 and 1); the phone got %+v", before, after, screen.inputs)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
