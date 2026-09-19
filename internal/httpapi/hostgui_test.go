package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/webui"
	"github.com/YKoustubhRao/flowera/internal/winhost"
)

// hostGUIServer returns a Server whose host helper is a bash loop speaking bridge.ps1's line protocol, plus the file
// it appends every received line to.
func hostGUIServer(t *testing.T, launchReply string) (*Server, string) {
	t.Helper()
	log := t.TempDir() + "/received.log"
	script := `while IFS= read -r l; do
  printf '%s\n' "$l" >> '` + log + `'
  case "$l" in
    launch*) echo '` + launchReply + `';;
    frame)   echo 'FRAME 4 3 QUJD';;
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
	t.Cleanup(srv.StopHostGUI)
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

func TestHostGUIStartInputStopAndStream(t *testing.T) {
	srv, log := hostGUIServer(t, "OK pid=9")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	tok := startHostGUI(t, srv, mux, "vm1-code", `C:\Apps\code.exe`, "--x")

	// The stream is real multipart/x-mixed-replace: read its first part like a browser's <img> would.
	ts := httptest.NewServer(mux)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/host-gui/stream?id=vm1-code&token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	mt, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/x-mixed-replace" {
		t.Fatalf("stream content type = %q (%v)", resp.Header.Get("Content-Type"), err)
	}
	part, err := multipart.NewReader(resp.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	jpeg, _ := io.ReadAll(part)
	if want, _ := base64.StdEncoding.DecodeString("QUJD"); !bytes.Equal(jpeg, want) || part.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("stream part = %q (%s)", jpeg, part.Header.Get("Content-Type"))
	}

	// The single-frame endpoint reports the size in headers.
	fr := httptest.NewRecorder()
	mux.ServeHTTP(fr, httptest.NewRequest(http.MethodGet, "/api/host-gui/frame?id=vm1-code&token="+tok, nil))
	if fr.Code != http.StatusOK || fr.Header().Get("X-Width") != "4" || fr.Header().Get("X-Height") != "3" {
		t.Fatalf("frame = %d %v", fr.Code, fr.Header())
	}

	for _, ev := range []map[string]any{
		{"id": "vm1-code", "kind": "down", "x": 5, "y": 6, "token": tok},
		{"id": "vm1-code", "kind": "up", "x": 5, "y": 6, "right": true, "token": tok},
		{"id": "vm1-code", "kind": "type", "text": "héllo", "token": tok},
		{"id": "vm1-code", "kind": "key", "vk": 67, "mods": "c", "token": tok},
		{"id": "vm1-code", "kind": "wheel", "x": 1, "y": 2, "delta": -1, "token": tok},
	} {
		if rec := hostGUIPost(t, mux, "/api/host-gui/input", ev); rec.Code != http.StatusNoContent {
			t.Fatalf("input %v = %d: %s", ev, rec.Code, rec.Body.String())
		}
	}
	time.Sleep(50 * time.Millisecond)
	data, _ := os.ReadFile(log)
	for _, want := range []string{"launch\tC:\\Apps\\code.exe\t--x", "down 5 6", "up 5 6 right", "type héllo", "key 67 c", "wheel 1 2 -1"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the host never received %q; got %q", want, data)
		}
	}

	if rec := hostGUIPost(t, mux, "/api/host-gui/stop", map[string]string{"id": "vm1-code", "token": tok}); rec.Code != http.StatusNoContent {
		t.Fatalf("stop = %d", rec.Code)
	}
	if rec := hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "vm1-code", "kind": "down", "token": tok}); rec.Code != http.StatusNotFound {
		t.Fatalf("input after stop = %d, want 404", rec.Code)
	}
}

func TestHostGUIStartShowsTheReasonAnAppCannotBeHidden(t *testing.T) {
	srv, _ := hostGUIServer(t, "ERR This is a Microsoft Store app. Windows only runs those on the visible desktop, so it cannot be hidden.")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

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
	tok := startHostGUI(t, srv, mux, "ok1", "a.exe", "")
	if rec := hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "ok1", "kind": "explode", "token": tok}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown input kind = %d, want 400", rec.Code)
	}
}

func TestHostGUIViewPage(t *testing.T) {
	srv, _ := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

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
	tok := startHostGUI(t, srv, mux, "work-vm-code", "a.exe", "")

	if _, ok := srv.Desktops.LastViewed("work-vm"); ok {
		t.Fatal("desktop already marked as viewed before any viewer connected")
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/host-gui/stream?id=work-vm-code&desktop=work-vm&token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	multipart.NewReader(resp.Body, "floweraframe").NextPart() // wait for the stream to be up

	if at, ok := srv.Desktops.LastViewed("work-vm"); !ok || time.Since(at) > 5*time.Second {
		t.Fatalf("LastViewed = %v, %v: idle-pause would freeze a desktop that is showing a host app", at, ok)
	}
}

func TestFrameLongPollWaitsForANewerPictureAndThenSaysNothingChanged(t *testing.T) {
	old := hostGUIPollWait
	hostGUIPollWait = 150 * time.Millisecond
	defer func() { hostGUIPollWait = old }()

	srv, _ := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	tok := startHostGUI(t, srv, mux, "poll1", "a.exe", "")

	get := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/host-gui/frame?id=poll1&token="+tok+q, nil))
		return rec
	}
	first := get("")
	if first.Code != http.StatusOK || first.Header().Get("X-Seq") != "1" {
		t.Fatalf("first frame = %d seq %q", first.Code, first.Header().Get("X-Seq"))
	}
	// The fake helper never changes its picture, so asking for anything newer than seq 1 must wait and then say so.
	begin := time.Now()
	again := get("&after=1")
	if again.Code != http.StatusNoContent || time.Since(begin) < 100*time.Millisecond {
		t.Fatalf("after=1 = %d in %v, want 204 after waiting", again.Code, time.Since(begin))
	}
	// Anything older than what exists is answered at once.
	if old := get("&after=0"); old.Code != http.StatusOK || old.Header().Get("X-Seq") != "1" {
		t.Fatalf("after=0 = %d seq %q, want the existing frame", old.Code, old.Header().Get("X-Seq"))
	}
	// A session that has gone is not "nothing changed": the viewer has to be told the app is closed.
	srv.hostGUI().Stop("poll1")
	if gone := get("&after=1"); gone.Code != http.StatusNotFound {
		t.Fatalf("frame after stop = %d, want 404", gone.Code)
	}
}

func TestHostGUIRefusesACallerWithoutTheLaunchToken(t *testing.T) {
	srv, log := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

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
	for name, code := range map[string]int{
		"input, no token":    hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "vm1-app", "kind": "type", "text": "x"}).Code,
		"input, wrong token": hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "vm1-app", "kind": "type", "text": "x", "token": "nope"}).Code,
		"stop, no token":     hostGUIPost(t, mux, "/api/host-gui/stop", map[string]string{"id": "vm1-app"}).Code,
	} {
		if code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", name, code)
		}
	}
	for _, path := range []string{"/api/host-gui/frame?id=vm1-app", "/api/host-gui/frame?id=vm1-app&token=nope", "/api/host-gui/stream?id=vm1-app"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403: knowing a session id must not show its screen", path, rec.Code)
		}
	}
	if !srv.hostGUI().Alive("vm1-app") {
		t.Fatal("a refused stop still ended the session")
	}
	if rec := hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "vm1-app", "kind": "type", "text": "x", "token": tok}); rec.Code != http.StatusNoContent {
		t.Fatalf("input with the right token = %d", rec.Code)
	}
}

func TestHostGUIRefusesRequestsThatCameThroughATunnelOrProxy(t *testing.T) {
	srv, _ := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	tok := startHostGUI(t, srv, mux, "vm1-app", "a.exe", "")

	for _, header := range []string{"Cf-Connecting-Ip", "Cf-Ray", "X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"} {
		for _, path := range []string{"/api/host-gui/frame?id=vm1-app&token=" + tok, "/api/host-gui/stream?id=vm1-app&token=" + tok, "/host-gui/view?id=vm1-app&token=" + tok} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set(header, "203.0.113.9")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("GET %s with %s = %d, want 403 even with a valid token", path, header, rec.Code)
			}
		}
		b, _ := json.Marshal(map[string]any{"id": "vm1-app", "kind": "type", "text": "x", "token": tok})
		req := httptest.NewRequest(http.MethodPost, "/api/host-gui/input", bytes.NewReader(b))
		req.Header.Set(header, "203.0.113.9")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("input with %s = %d, want 403", header, rec.Code)
		}
	}
}

func TestHostGUICopyReturnsWhatTheAppCopiedAndPasteTypesIt(t *testing.T) {
	srv, log := hostGUIServer(t, "OK pid=1")
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	tok := startHostGUI(t, srv, mux, "clip1", "a.exe", "")

	rec := hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "clip1", "kind": "copy", "vk": 67, "token": tok})
	var body struct{ Text string }
	if json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusOK || body.Text != "hi" {
		t.Fatalf("copy = %d %s, want the text the app copied", rec.Code, rec.Body.String())
	}
	if rec := hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "clip1", "kind": "copy", "vk": 86, "token": tok}); rec.Code != http.StatusBadGateway {
		t.Errorf("a 'copy' of Ctrl+V = %d, want it refused", rec.Code)
	}
	if rec := hostGUIPost(t, mux, "/api/host-gui/input", map[string]any{"id": "clip1", "kind": "paste", "text": "a\nb", "token": tok}); rec.Code != http.StatusNoContent {
		t.Fatalf("paste = %d", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	data, _ := os.ReadFile(log)
	for _, want := range []string{"copy 67", "type a", "key 13", "type b"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the host never received %q; got %q", want, data)
		}
	}
}
