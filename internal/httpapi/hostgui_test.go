package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// hostGUIServer returns a Server whose host helper is a bash loop speaking bridge.ps1's line protocol, plus the file
// it appends every received line to.
func hostGUIServer(t *testing.T, launchReply string) (*Server, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helper is faked with bash")
	}
	// Not t.TempDir(): the fake helper can still be appending a line when the test ends, and the harness's own removal of
	// a folder that is being written into fails the test. Removing it here, after the helper is stopped, does not care.
	dir, err := os.MkdirTemp("", "hostgui")
	if err != nil {
		t.Fatal(err)
	}
	log := dir + "/received.log"
	script := `while IFS= read -r l; do
  printf '%s\n' "$l" >> '` + log + `'
  case "$l" in
    launch*) echo '` + launchReply + `'; case '` + launchReply + `' in OK*) printf 'FRAME 4 3 0 0 4 3 1 3\nABC';; esac;;
    keyframe) echo OK; printf 'FRAME 4 3 0 0 4 3 1 3\nABC';;
    "emit patch") echo OK; printf 'FRAME 4 3 1 1 2 2 0 2\nXY';;
    status)  echo 'STATUS running';;
    "copy 67") echo 'CLIP aGk=';;
    quit)    exit 0;;
    *)       echo OK;;
  esac
done`
	srv := &Server{Web: webui.NewRenderer()}
	srv.hostGUIOnce.Do(func() {
		srv.hostGUIMgr = winhost.NewManager(func(ctx context.Context, name string) (*exec.Cmd, error) {
			return exec.Command("bash", "-c", script), nil
		})
	})
	t.Cleanup(func() {
		srv.StopHostGUI()
		time.Sleep(50 * time.Millisecond)
		os.RemoveAll(dir)
	})
	return srv, log
}

// startHostGUI starts an app the way a launcher's viewer would - with the token signed for exactly that app - and returns
// the token, which every later request has to present.
func startHostGUI(t *testing.T, srv *Server, mux http.Handler, id, target, args string) string {
	t.Helper()
	tok := srv.hostGUIToken(id, target, args)
	if rec := hostGUIPost(t, mux, "/api/host-gui/start", map[string]string{"id": id, "target": target, "args": args, "token": tok}); rec.Code != http.StatusOK {
		t.Fatalf("start %s = %d: %s", id, rec.Code, rec.Body.String())
	}
	return tok
}

func hostGUIPost(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// testWS is a browser's end of a WebSocket: what it sends is masked, what it receives is not.
type testWS struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

// dialWS opens a WebSocket to path on ts. A refused handshake is returned as the HTTP response instead of a connection.
func dialWS(t *testing.T, ts *httptest.Server, path string, header map[string]string) (*testWS, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	rand.Read(key)
	host := ts.Listener.Addr().String()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n"
	if _, ok := header["Origin"]; !ok {
		req += "Origin: http://" + host + "\r\n"
	}
	for k, v := range header {
		req += k + ": " + v + "\r\n"
	}
	io.WriteString(conn, req+"\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, resp
	}
	c := &testWS{t: t, conn: conn, br: br}
	t.Cleanup(func() { conn.Close() })
	return c, resp
}

func (c *testWS) send(op byte, payload []byte) {
	c.t.Helper()
	mask := []byte{1, 2, 3, 4}
	frame := []byte{0x80 | op}
	switch l := len(payload); {
	case l < 126:
		frame = append(frame, 0x80|byte(l))
	case l < 1<<16:
		frame = append(frame, 0x80|126, byte(l>>8), byte(l))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(l))
	}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testWS) sendJSON(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	c.send(wsText, b)
}

// recv returns the next data message, skipping pings.
func (c *testWS) recv() (op byte, payload []byte, err error) {
	for {
		c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var h [2]byte
		if _, err = io.ReadFull(c.br, h[:]); err != nil {
			return 0, nil, err
		}
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			io.ReadFull(c.br, b[:])
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			io.ReadFull(c.br, b[:])
			n = binary.BigEndian.Uint64(b[:])
		}
		payload = make([]byte, n)
		if _, err = io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		if h[0]&0x0F == wsPing {
			continue
		}
		return h[0] & 0x0F, payload, nil
	}
}

func (c *testWS) mustRecv() (byte, []byte) {
	c.t.Helper()
	op, p, err := c.recv()
	if err != nil {
		c.t.Fatalf("waiting for a message: %v", err)
	}
	return op, p
}

// picture is a decoded binary message: flags, picture size, rectangle, JPEG.
type picture struct {
	Whole              bool
	W, H, X, Y, Rw, Rh int
	JPEG               []byte
}

func (c *testWS) mustPicture() picture {
	c.t.Helper()
	op, p := c.mustRecv()
	if op != wsBinary || len(p) < 13 {
		c.t.Fatalf("not a picture: op %d, %d bytes", op, len(p))
	}
	u := func(i int) int { return int(binary.BigEndian.Uint16(p[1+2*i:])) }
	return picture{Whole: p[0]&1 == 1, W: u(0), H: u(1), X: u(2), Y: u(3), Rw: u(4), Rh: u(5), JPEG: p[13:]}
}

func TestHostGUIStartStreamInputAndStop(t *testing.T) {
	srv, log := hostGUIServer(t, "OK pid=9")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)
	tok := startHostGUI(t, srv, mux, "vm1-code", `C:\Apps\code.exe`, "--x")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ws, resp := dialWS(t, ts, "/api/host-gui/ws?id=vm1-code&token="+tok, nil)
	if ws == nil {
		t.Fatalf("handshake = %d", resp.StatusCode)
	}

	// The first thing a viewer gets is the whole picture, whether or not the app has changed.
	if p := ws.mustPicture(); !p.Whole || p.W != 4 || p.H != 3 || p.X != 0 || p.Y != 0 || p.Rw != 4 || p.Rh != 3 || string(p.JPEG) != "ABC" {
		t.Fatalf("first message: %+v", p)
	}
	// Later, only what changed.
	sess, _ := srv.hostGUI().Get("vm1-code")
	if _, err := sess.Do("emit patch"); err != nil {
		t.Fatal(err)
	}
	if p := ws.mustPicture(); p.Whole || p.X != 1 || p.Y != 1 || p.Rw != 2 || p.Rh != 2 || string(p.JPEG) != "XY" {
		t.Fatalf("patch: %+v", p)
	}

	for _, ev := range []map[string]any{
		{"kind": "down", "x": 5, "y": 6},
		{"kind": "up", "x": 5, "y": 6, "right": true},
		{"kind": "type", "text": "héllo"},
		{"kind": "key", "vk": 67, "mods": "c"},
		{"kind": "wheel", "x": 1, "y": 2, "delta": -1},
	} {
		ws.sendJSON(ev)
	}
	waitFor(t, 3*time.Second, "the host to receive every input, in order", func() bool {
		data, _ := os.ReadFile(log)
		return inOrder(string(data), "launch\tC:\\Apps\\code.exe\t--x", "down 5 6", "up 5 6 right", "type héllo", "key 67 c", "wheel 1 2 -1")
	})

	if rec := hostGUIPost(t, mux, "/api/host-gui/stop", map[string]string{"id": "vm1-code", "token": tok}); rec.Code != http.StatusNoContent {
		t.Fatalf("stop = %d", rec.Code)
	}
	// The viewer is told, so it can say the app was closed.
	if op, p, err := ws.recv(); err != nil || op != wsText || !strings.Contains(string(p), `"closed":true`) {
		t.Fatalf("after stop: op %d %q err %v, want {\"closed\":true}", op, p, err)
	}
	if _, resp := dialWS(t, ts, "/api/host-gui/ws?id=vm1-code&token="+tok, nil); resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("connecting after stop = %v, want 404", resp)
	}
}

func inOrder(text string, parts ...string) bool {
	at := 0
	for _, p := range parts {
		i := strings.Index(text[at:], p)
		if i < 0 {
			return false
		}
		at += i + len(p)
	}
	return true
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostGUIStartShowsTheReasonAnAppCannotBeHidden(t *testing.T) {
	srv, _ := hostGUIServer(t, "ERR This is a Microsoft Store app. Windows only runs those on the visible desktop, so it cannot be hidden.")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)

	target := `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`
	rec := hostGUIPost(t, mux, "/api/host-gui/start", map[string]string{"id": "vm1-calc", "target": target, "token": srv.hostGUIToken("vm1-calc", target, "")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("start = %d, want 422", rec.Code)
	}
	var body struct{ Error string }
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "Microsoft Store app") {
		t.Fatalf("error = %q: the viewer has to be able to tell the person why", body.Error)
	}
}

func TestHostGUIRejectsBadRequests(t *testing.T) {
	srv, _ := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)

	for _, id := range []string{"", "../x", "a b", strings.Repeat("a", 65), "a;b"} {
		if rec := hostGUIPost(t, mux, "/api/host-gui/start", map[string]string{"id": id, "target": "a.exe", "token": srv.hostGUIToken(id, "a.exe", "")}); rec.Code != http.StatusBadRequest {
			t.Errorf("start with id %q = %d, want 400", id, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/host-gui/start", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET start = %d, want 405", rec.Code)
	}
}

func TestHostGUIIgnoresInputItDoesNotUnderstandAndKeepsGoing(t *testing.T) {
	srv, log := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)
	tok := startHostGUI(t, srv, mux, "ok1", "a.exe", "")
	ts := httptest.NewServer(mux)
	defer ts.Close()
	ws, _ := dialWS(t, ts, "/api/host-gui/ws?id=ok1&token="+tok, nil)
	ws.mustRecv()

	ws.send(wsText, []byte("not json"))
	ws.sendJSON(map[string]any{"kind": "explode"})
	ws.send(wsBinary, []byte{1, 2, 3})
	ws.sendJSON(map[string]any{"kind": "key", "vk": 0})               // out of range
	ws.sendJSON(map[string]any{"kind": "key", "vk": 65, "mods": "x"}) // not a modifier
	ws.sendJSON(map[string]any{"kind": "type", "text": "still here"})
	waitFor(t, 3*time.Second, "the input after the bad ones", func() bool {
		data, _ := os.ReadFile(log)
		return strings.Contains(string(data), "type still here")
	})
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "explode") || strings.Contains(string(data), "key 65 x") {
		t.Fatalf("the host was sent input the control plane should have refused: %q", data)
	}
}

func TestHostGUIViewPage(t *testing.T) {
	srv, _ := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/host-gui/view?id=vm1-code&title=Code&target=C%3A%5CApps%5Ccode.exe", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `ID = "vm1-code"`) {
		t.Fatalf("view = %d\n%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/host-gui/view?id=bad%20id", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("view with a bad id = %d, want 400", rec.Code)
	}
}

func TestAnOpenViewerKeepsItsDesktopFromIdlePausing(t *testing.T) {
	srv, _ := hostGUIServer(t, "OK pid=1")
	srv.Desktops = &desktop.Manager{}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)
	tok := startHostGUI(t, srv, mux, "work-vm-code", "a.exe", "")

	if _, ok := srv.Desktops.LastViewed("work-vm"); ok {
		t.Fatal("desktop already marked as viewed before any viewer connected")
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()
	ws, _ := dialWS(t, ts, "/api/host-gui/ws?id=work-vm-code&desktop=work-vm&token="+tok, nil)
	ws.mustRecv() // wait for the connection to be up

	if at, ok := srv.Desktops.LastViewed("work-vm"); !ok || time.Since(at) > 5*time.Second {
		t.Fatalf("LastViewed = %v, %v: idle-pause would freeze a desktop that is showing a host app", at, ok)
	}
}

func TestHostGUIRefusesACallerWithoutTheLaunchToken(t *testing.T) {
	srv, log := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)

	target := `C:\Program Files\App\app.exe`
	good := srv.hostGUIToken("vm1-app", target, "")

	// Starting: no token, a token for a different program, a token for a different session, a guess.
	for name, body := range map[string]map[string]string{
		"no token":                {"id": "vm1-app", "target": target},
		"a token for another app": {"id": "vm1-app", "target": `C:\Windows\System32\cmd.exe`, "token": good},
		"a token for a session id you were not given": {"id": "vm2-app", "target": target, "token": good},
		"a made-up token": {"id": "vm1-app", "target": target, "token": "0123456789abcdef0123456789abcdef"},
	} {
		if rec := hostGUIPost(t, mux, "/api/host-gui/start", body); rec.Code != http.StatusForbidden {
			t.Errorf("start with %s = %d, want 403", name, rec.Code)
		}
	}
	if data, _ := os.ReadFile(log); strings.Contains(string(data), "launch") {
		t.Fatalf("the host was asked to launch something for a caller with no valid token: %q", data)
	}

	// A running session's id alone gets nothing: every later request must present the token it was started with.
	tok := startHostGUI(t, srv, mux, "vm1-app", target, "")
	if rec := hostGUIPost(t, mux, "/api/host-gui/stop", map[string]string{"id": "vm1-app"}); rec.Code != http.StatusForbidden {
		t.Errorf("stop with no token = %d, want 403", rec.Code)
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()
	for _, path := range []string{"/api/host-gui/ws?id=vm1-app", "/api/host-gui/ws?id=vm1-app&token=nope"} {
		if ws, resp := dialWS(t, ts, path, nil); ws != nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("WebSocket %s = %v, want 403: knowing a session id must not show its screen or drive it", path, resp)
		}
	}
	if !srv.hostGUI().Alive("vm1-app") {
		t.Fatal("a refused stop still ended the session")
	}
	if ws, resp := dialWS(t, ts, "/api/host-gui/ws?id=vm1-app&token="+tok, nil); ws == nil {
		t.Fatalf("connecting with the right token = %d", resp.StatusCode)
	}
}

func TestHostGUIRefusesAPageFromAnotherOrigin(t *testing.T) {
	srv, _ := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)
	tok := startHostGUI(t, srv, mux, "vm1-app", "a.exe", "")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	if ws, resp := dialWS(t, ts, "/api/host-gui/ws?id=vm1-app&token="+tok, map[string]string{"Origin": "http://evil.example"}); ws != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a WebSocket from another site = %v, want 403: any page you visit could otherwise drive the app", resp)
	}
}

func TestHostGUIRefusesRequestsThatCameThroughATunnelOrProxy(t *testing.T) {
	srv, _ := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)
	tok := startHostGUI(t, srv, mux, "vm1-app", "a.exe", "")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, header := range []string{"Cf-Connecting-Ip", "Cf-Ray", "X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"} {
		req := httptest.NewRequest(http.MethodGet, "/host-gui/view?id=vm1-app&token="+tok, nil)
		req.Header.Set(header, "203.0.113.9")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("view with %s = %d, want 403 even with a valid token", header, rec.Code)
		}
		if ws, resp := dialWS(t, ts, "/api/host-gui/ws?id=vm1-app&token="+tok, map[string]string{header: "203.0.113.9"}); ws != nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("WebSocket with %s = %v, want 403", header, resp)
		}
	}
}

func TestHostGUICopyReturnsWhatTheAppCopiedAndPasteTypesIt(t *testing.T) {
	srv, log := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterLocalRoutes(mux)
	tok := startHostGUI(t, srv, mux, "clip1", "a.exe", "")
	ts := httptest.NewServer(mux)
	defer ts.Close()
	ws, _ := dialWS(t, ts, "/api/host-gui/ws?id=clip1&token="+tok, nil)
	ws.mustRecv()

	ws.sendJSON(map[string]any{"kind": "copy", "vk": 67, "seq": 7})
	op, p := ws.mustRecv()
	var ans struct {
		ID   int
		Text string
	}
	if json.Unmarshal(p, &ans); op != wsText || ans.ID != 7 || ans.Text != "hi" {
		t.Fatalf("copy answer = %d %s, want the text the app copied, under the request's own id", op, p)
	}
	ws.sendJSON(map[string]any{"kind": "copy", "vk": 86, "seq": 8}) // Ctrl+V is not a copy
	ws.sendJSON(map[string]any{"kind": "paste", "text": "a\nb"})
	waitFor(t, 3*time.Second, "the pasted text to reach the host", func() bool {
		data, _ := os.ReadFile(log)
		return inOrder(string(data), "copy 67", "type a", "key 13", "type b")
	})
	if data, _ := os.ReadFile(log); strings.Contains(string(data), "copy 86") {
		t.Fatal("a 'copy' of Ctrl+V reached the host: only Ctrl+C and Ctrl+X may read the person's clipboard")
	}
}
