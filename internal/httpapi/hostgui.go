package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/winhost"
	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// hostGUIDir is where the Windows helper script is written. It has to be somewhere the host can read: docker-compose.yaml
// mounts the host's /tmp at the same path, so writing here from the control plane's container lands it on the host.
const hostGUIDir = "/tmp/decurix-winhost"

var hostGUIID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// hostGUI returns the manager for apps running on a private desktop of the host, creating it on first use.
func (s *Server) hostGUI() *winhost.Manager {
	s.hostGUIOnce.Do(func() { s.hostGUIMgr = winhost.NewManager(hostGUIStarter) })
	return s.hostGUIMgr
}

// hostGUISecretKey returns the key launch tokens are signed with: the one main.go loaded from the data directory, or, for
// a Server built without one (tests), a random key for this process.
func (s *Server) hostGUISecretKey() []byte {
	s.hostGUIAuthMu.Lock()
	defer s.hostGUIAuthMu.Unlock()
	if len(s.HostGUISecret) == 0 {
		s.HostGUISecret = make([]byte, 32)
		rand.Read(s.HostGUISecret)
	}
	return s.HostGUISecret
}

// hostGUIToken is the launch token for one app on one session, as a launcher would carry it.
func (s *Server) hostGUIToken(id, target, args string) string {
	return winhost.Token(s.hostGUISecretKey(), id, target, args)
}

// hostGUIAuthorised reports whether token is the one this session was started with. Only start checks a token against
// what it launches; every later request has to present the same token, so knowing a session's id is not enough.
func (s *Server) hostGUIAuthorised(id, token string) bool {
	s.hostGUIAuthMu.Lock()
	defer s.hostGUIAuthMu.Unlock()
	want, ok := s.hostGUIAuth[id]
	return ok && token != "" && subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

func (s *Server) hostGUIAuthorise(id, token string) {
	s.hostGUIAuthMu.Lock()
	defer s.hostGUIAuthMu.Unlock()
	if s.hostGUIAuth == nil {
		s.hostGUIAuth = make(map[string]string)
	}
	s.hostGUIAuth[id] = token
}

// hostGUIDirectOnly refuses a request that came through a reverse proxy or tunnel. The viewer runs in a window of the VM
// and reaches the control plane directly; a request that arrives with forwarding headers came from further away than
// any desktop, and this is the one part of the API that lets a caller see and drive a program on the host.
func hostGUIDirectOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"} {
			if r.Header.Get(h) != "" {
				http.Error(w, "not available through a tunnel or proxy", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// StopHostGUI ends every app running on a private host desktop. Those apps have no window anywhere else, so none
// may outlive the control plane that is showing them.
func (s *Server) StopHostGUI() {
	s.hostGUI().StopAll()
}

// hostGUISupported reports whether this control plane can run the Windows helper at all: on Windows itself, or on
// Linux reaching Windows through WSL. A Mac or an ordinary Linux machine has no private Win32 desktop to run an app on.
func hostGUISupported() bool {
	return desktop.IsWSLOrWindows()
}

// writeHostGUIScript puts bridge.ps1 in dir (unless it is already there, unchanged) and returns its path.
func writeHostGUIScript(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("prepare host helper: %w", err)
	}
	script := filepath.Join(dir, "bridge.ps1")
	if old, err := os.ReadFile(script); err != nil || !bytes.Equal(old, winhost.Script) {
		if err := os.WriteFile(script, winhost.Script, 0o644); err != nil {
			return "", fmt.Errorf("write host helper: %w", err)
		}
	}
	return script, nil
}

// hostGUIStarter returns the command that runs the Windows helper script on the host, keeping its stdin open so the
// control plane can drive it.
func hostGUIStarter(ctx context.Context, desktopName string) (*exec.Cmd, error) {
	if !hostGUISupported() {
		return nil, errors.New("Windows apps can only be shown when Decurix runs on a Windows computer")
	}
	if runtime.GOOS == "windows" {
		return nativeHostGUICmd(ctx, desktopName)
	}
	script, err := writeHostGUIScript(hostGUIDir)
	if err != nil {
		return nil, err
	}
	// desktopName is already reduced to [A-Za-z0-9_] by winhost, so it is safe in a shell word.
	shell := fmt.Sprintf(`PATH="$PATH:/mnt/c/Windows/System32:/mnt/c/Windows/System32/WindowsPowerShell/v1.0" `+
		`powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$(wslpath -w %s)" -DesktopName %s`, script, desktopName)
	return hostShellCmd(ctx, shell), nil
}

// nativeHostGUICmd runs the helper directly, for a control plane that is itself a Windows program: no shell and no
// path translation, just Windows PowerShell 5.1 (the one every Windows machine has) on a script in the temp folder.
func nativeHostGUICmd(ctx context.Context, desktopName string) (*exec.Cmd, error) {
	ps := desktop.PowershellExe()
	if ps == "" {
		return nil, errors.New("Windows PowerShell was not found on this computer")
	}
	script, err := writeHostGUIScript(filepath.Join(os.TempDir(), "decurix-winhost"))
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-DesktopName", desktopName), nil
}

// warmHostGUI starts (or keeps) the helper the next host app will run in. It only makes sense where host apps can run at
// all: on Windows, or on Linux reaching Windows through WSL.
func (s *Server) warmHostGUI() {
	if !hostGUISupported() {
		return
	}
	s.hostGUI().Warm()
}

// checkHostApps asks the Windows helper which programs it can run on a private desktop, without starting any of them. It
// uses the same warm helper an app is launched into, so what it learns about the Start menu is not learned twice.
func (s *Server) checkHostApps(ctx context.Context, targets []winhost.Target) (map[winhost.Target]string, error) {
	return s.hostGUI().Check(ctx, targets)
}

// keptHostApp is one app kept running and ready on the host (winhost.Manager.Keep), as saved in the data directory so a
// restart of the control plane keeps it ready too.
type keptHostApp struct {
	Desktop string `json:"desktop"`
	ID      string `json:"id"`
	Target  string `json:"target"`
	Args    string `json:"args"`
}

// keptHostAppsFile is where the apps kept ready are saved, in HostGUIDataDir.
const keptHostAppsFile = "hostgui-apps.json"

// startHostGUIAppAhead keeps a host_gui app running on the host, under the same session id its packaged launcher will
// ask for, from the moment that launcher is written into a VM - at desktop creation or a later app copy - rather than
// the moment a person opens it. winhost.Manager.Start keys its session by id, so the viewer's own POST to
// /api/host-gui/start finds the app already running and joins it at once; and when the person closes it, a fresh one
// is started for next time.
func (s *Server) startHostGUIAppAhead(desktopID, id, target, args string) {
	if !hostGUIID.MatchString(id) || !hostGUISupported() {
		return
	}
	s.hostGUI().Keep(desktopID, id, target, args)

	s.keptHostAppsMu.Lock()
	defer s.keptHostAppsMu.Unlock()
	if s.keptHostApps == nil {
		s.keptHostApps = make(map[string]keptHostApp)
	}
	s.keptHostApps[id] = keptHostApp{Desktop: desktopID, ID: id, Target: target, Args: args}
	s.saveKeptHostAppsLocked()
}

// forgetHostApps ends the apps kept ready for a VM that is being deleted: nobody can open them any more, and an app on a
// private desktop is invisible to the person who would otherwise close it.
func (s *Server) forgetHostApps(desktopID string) {
	if hostGUISupported() {
		s.hostGUI().ForgetDesktop(desktopID)
	}
	s.keptHostAppsMu.Lock()
	defer s.keptHostAppsMu.Unlock()
	changed := false
	for id, app := range s.keptHostApps {
		if app.Desktop == desktopID {
			delete(s.keptHostApps, id)
			changed = true
		}
	}
	if changed {
		s.saveKeptHostAppsLocked()
	}
}

// saveKeptHostAppsLocked writes the apps kept ready to the data directory; keptHostAppsMu must be held. Best-effort: a
// failure only means a restart starts them when they are first opened instead of ahead of time.
func (s *Server) saveKeptHostAppsLocked() {
	if s.HostGUIDataDir == "" {
		return
	}
	apps := make([]keptHostApp, 0, len(s.keptHostApps))
	for _, app := range s.keptHostApps {
		apps = append(apps, app)
	}
	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(s.HostGUIDataDir, keptHostAppsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("host apps: could not save the apps kept ready: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("host apps: could not save the apps kept ready: %v", err)
	}
}

// RestoreHostApps starts again, in the background, every app that was kept ready when the control plane last ran, for
// the VMs that still exist. main calls it once HostGUIDataDir is set.
func (s *Server) RestoreHostApps(ctx context.Context) {
	if s.HostGUIDataDir == "" || !hostGUISupported() {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.HostGUIDataDir, keptHostAppsFile))
	if err != nil {
		return
	}
	var apps []keptHostApp
	if err := json.Unmarshal(data, &apps); err != nil {
		log.Printf("host apps: could not read the apps kept ready: %v", err)
		return
	}

	exists := map[string]bool{}
	listed := false
	if s.Desktops != nil {
		if desktops, err := s.Desktops.List(ctx); err == nil {
			listed = true
			for _, d := range desktops {
				exists[d.ID] = true
			}
		}
	}

	s.keptHostAppsMu.Lock()
	defer s.keptHostAppsMu.Unlock()
	if s.keptHostApps == nil {
		s.keptHostApps = make(map[string]keptHostApp)
	}
	for _, app := range apps {
		if !hostGUIID.MatchString(app.ID) || (listed && !exists[app.Desktop]) {
			continue
		}
		s.keptHostApps[app.ID] = app
		s.hostGUI().Keep(app.Desktop, app.ID, app.Target, app.Args)
	}
	s.saveKeptHostAppsLocked() // drops the apps of VMs deleted while the control plane was down
}

// hostShellCmd builds a command that runs a shell script on the host with stdin left open for the caller: directly when
// the control plane is on the host itself, through the same docker/nsenter hop createHostCmd uses when it is in a container.
func hostShellCmd(ctx context.Context, script string) *exec.Cmd {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		if _, err := os.Stat("/var/run/docker.sock"); err == nil {
			if dockerBin := findDockerBinary(); dockerBin != "" {
				return exec.CommandContext(ctx, dockerBin, "run", "-i", "--rm", "--privileged", "--pid=host", "alpine",
					"nsenter", "-t", "1", "-m", "-u", "-n", "-i", "bash", "-c", script)
			}
		}
	}
	return exec.CommandContext(ctx, "bash", "-c", script)
}

// hostGUIRequest is the body of every host-gui POST.
type hostGUIRequest struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Target string `json:"target"`
	Args   string `json:"args"`

	// Input events.
	Kind  string `json:"kind"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	Right bool   `json:"right"`
	Delta int    `json:"delta"`
	Text  string `json:"text"`
	VK    int    `json:"vk"`
	// Seq names a request that is answered (a copy), so the viewer can match the answer to it.
	Seq int `json:"seq,omitempty"`
	// Mods is any of c, s, a: Ctrl, Shift and Alt held during a key or mouse event.
	Mods string `json:"mods"`
}

func readHostGUIRequest(w http.ResponseWriter, r *http.Request) (hostGUIRequest, bool) {
	var req hostGUIRequest
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return req, false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return req, false
	}
	if !hostGUIID.MatchString(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// hostGUIStartHandler starts an app on a private host desktop (or joins the one already running under that id) and
// answers with the size of its window. A failure comes back as {"error": "..."} because what went wrong ("Store apps
// cannot be hidden", "the app closed straight away") is exactly what the viewer has to tell the person.
func (s *Server) hostGUIStartHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := readHostGUIRequest(w, r)
	if !ok {
		return
	}
	if !winhost.ValidToken(s.hostGUISecretKey(), req.ID, req.Target, req.Args, req.Token) {
		http.Error(w, "this launcher is not authorised to start that app", http.StatusForbidden)
		return
	}
	s.hostGUIAuthorise(req.ID, req.Token)
	sess, err := s.hostGUI().Start(r.Context(), req.ID, req.Target, req.Args)
	if err != nil {
		log.Printf("host-gui: start %s: %v", req.ID, err)
		w.WriteHeader(http.StatusUnprocessableEntity)
		jsonResponse(w, map[string]string{"error": err.Error()})
		return
	}
	width, height := sess.Size()
	jsonResponse(w, map[string]any{"id": req.ID, "w": width, "h": height})
}

// markDesktopViewed tells idle-pause that a desktop is in use for as long as ctx lasts. The desktop is showing a host app
// in a window of its own browser, which is not a thumbnail or a proxied screen, so nothing else would say so.
func (s *Server) markDesktopViewed(ctx context.Context, desktopID string) {
	if s.Desktops == nil || !hostGUIID.MatchString(desktopID) {
		return
	}
	s.Desktops.MarkViewed(desktopID)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Desktops.MarkViewed(desktopID)
			}
		}
	}()
}

// hostGUIWSHandler is the viewer's one connection to a running app: pictures flow out as the app changes, and input
// flows in as the person makes it. Both directions are ordered and neither waits for the other, which is why a keystroke
// does not sit behind a picture (and a picture is never requested - the host sends one when something has changed).
//
// Server to viewer, binary: one flags byte (bit 0: the rectangle is the whole picture), then six big-endian uint16 - the
// picture's width and height and the rectangle's x, y, width and height - then the rectangle as a JPEG. Text messages
// out are JSON: {"closed":true} once the app has no window left, and {"id":n,"text":"..."} answering a copy.
// Viewer to server, text JSON, one input each (see applyInput); a copy carries an "id" to be answered under.
func (s *Server) hostGUIWSHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !s.hostGUIAuthorised(q.Get("id"), q.Get("token")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sess, ok := s.hostGUI().Get(q.Get("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	c, err := wsconn.Upgrade(w, r)
	if err != nil {
		return
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.markDesktopViewed(ctx, q.Get("desktop"))
	v := sess.Watch()
	defer v.Close()

	// Everything written goes through this one goroutine's loop plus the copy answers, which share the connection's lock.
	go func() {
		defer cancel()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case u, ok := <-v.C:
				if !ok {
					return
				}
				if c.Write(wsconn.OpBinary, encodePatch(u)) != nil {
					return
				}
			case <-sess.AppClosed():
				c.Write(wsconn.OpText, []byte(`{"closed":true}`))
				return
			case <-sess.Done():
				c.Write(wsconn.OpText, []byte(`{"closed":true}`))
				return
			case <-ping.C:
				if c.Write(wsconn.OpPing, nil) != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		c.Close() // unblocks the read below
	}()

	for {
		op, msg, err := c.Read(70 * time.Second) // the viewer answers pings, so a quiet line is a dead one
		if err != nil {
			return
		}
		if op != wsconn.OpText {
			continue
		}
		var req hostGUIRequest
		if json.Unmarshal(msg, &req) != nil {
			continue
		}
		text, wantsText, err := applyInput(sess, req)
		if err != nil {
			log.Printf("host-gui: input %s on %s: %v", req.Kind, q.Get("id"), err)
			continue
		}
		if wantsText {
			out, _ := json.Marshal(map[string]any{"id": req.Seq, "text": text})
			if c.Write(wsconn.OpText, out) != nil {
				return
			}
		}
	}
}

// encodePatch is the binary form of an Update the viewer receives.
func encodePatch(u winhost.Update) []byte {
	b := make([]byte, 13, 13+len(u.JPEG))
	if u.Whole {
		b[0] = 1
	}
	for i, v := range []int{u.W, u.H, u.X, u.Y, u.Rw, u.Rh} {
		binary.BigEndian.PutUint16(b[1+2*i:], uint16(v))
	}
	return append(b, u.JPEG...)
}

// applyInput carries out one input from the viewer on the app. wantsText is set for a copy, whose answer is the text
// the app put on the clipboard.
func applyInput(sess *winhost.Session, req hostGUIRequest) (text string, wantsText bool, err error) {
	switch req.Kind {
	case "move", "down", "up":
		err = sess.Mouse(req.Kind, req.X, req.Y, req.Right, req.Mods)
	case "wheel":
		err = sess.Wheel(req.X, req.Y, req.Delta)
	case "type":
		err = sess.Type(req.Text)
	case "paste":
		err = sess.Paste(req.Text)
	case "copy":
		text, err = sess.Copy(req.VK)
		return text, err == nil, err
	case "key":
		err = sess.Key(req.VK, req.Mods)
	default:
		err = errors.New("unknown input kind")
	}
	return "", false, err
}

// hostGUIStopHandler ends the session, closing the app on the host.
func (s *Server) hostGUIStopHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := readHostGUIRequest(w, r)
	if !ok {
		return
	}
	if !s.hostGUIAuthorised(req.ID, req.Token) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.hostGUI().Stop(req.ID)
	w.WriteHeader(http.StatusNoContent)
}

// hostGUIViewHandler serves the page a desktop's launcher opens: a window that shows the host app and sends the
// person's mouse and keyboard back to it.
func (s *Server) hostGUIViewHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	if !hostGUIID.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(q.Get("title"))
	if title == "" {
		title = "Host app"
	}
	var out bytes.Buffer
	if err := s.Web.RenderHostGUI(&out, webui.HostGUIPageData{ID: id, Token: q.Get("token"), Desktop: q.Get("desktop"), Title: title, Target: q.Get("target"), Args: q.Get("args")}); err != nil {
		jsonError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out.Bytes())
}
