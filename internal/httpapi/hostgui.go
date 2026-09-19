package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
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
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/webui"
	"github.com/YKoustubhRao/flowera/internal/winhost"
)

// hostGUIDir is where the Windows helper script is written. It has to be somewhere the host can read: docker-compose.yaml
// mounts the host's /tmp at the same path, so writing here from the control plane's container lands it on the host.
const hostGUIDir = "/tmp/flowera-winhost"

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

// hostGUIStarter returns the command that runs the Windows helper script on the host, keeping its stdin open so the
// control plane can drive it.
func hostGUIStarter(ctx context.Context, desktopName string) (*exec.Cmd, error) {
	if runtime.GOOS == "windows" || !desktop.IsWSLOrWindows() {
		return nil, errors.New("Windows apps can only be shown when Flowera runs on Windows (through WSL)")
	}
	if err := os.MkdirAll(hostGUIDir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare host helper: %w", err)
	}
	script := filepath.Join(hostGUIDir, "bridge.ps1")
	if old, err := os.ReadFile(script); err != nil || !bytes.Equal(old, winhost.Script) {
		if err := os.WriteFile(script, winhost.Script, 0o644); err != nil {
			return nil, fmt.Errorf("write host helper: %w", err)
		}
	}
	// desktopName is already reduced to [A-Za-z0-9_] by winhost, so it is safe in a shell word.
	shell := fmt.Sprintf(`PATH="$PATH:/mnt/c/Windows/System32:/mnt/c/Windows/System32/WindowsPowerShell/v1.0" `+
		`powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$(wslpath -w %s)" -DesktopName %s`, script, desktopName)
	return hostShellCmd(ctx, shell), nil
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
	f, _ := sess.Latest()
	jsonResponse(w, map[string]any{"id": req.ID, "w": f.W, "h": f.H})
}

// hostGUIStreamHandler streams the app's window as motion JPEG (multipart/x-mixed-replace), which a plain <img> plays.
// Frames are sent when the picture changes, so an idle app costs nothing.
func (s *Server) hostGUIStreamHandler(w http.ResponseWriter, r *http.Request) {
	if !s.hostGUIAuthorised(r.URL.Query().Get("id"), r.URL.Query().Get("token")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sess, ok := s.hostGUI().Get(r.URL.Query().Get("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sess.Watch(r.Context())
	s.markDesktopViewed(r.Context(), r.URL.Query().Get("desktop"))
	const boundary = "floweraframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store")

	// The boundary follows each frame's data rather than preceding the next frame's headers, so a part is complete
	// (and can be drawn) the moment it is sent - an app that stops changing must not leave its last picture unfinished.
	fmt.Fprintf(w, "--%s\r\n", boundary)
	flusher.Flush()

	var seq uint64
	for {
		sess.Touch()
		f, err := sess.WaitFrame(r.Context(), seq)
		if err != nil {
			return
		}
		seq = f.Seq
		fmt.Fprintf(w, "Content-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(f.JPEG))
		if _, err := w.Write(f.JPEG); err != nil {
			return
		}
		fmt.Fprintf(w, "\r\n--%s\r\n", boundary)
		flusher.Flush()
	}
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

// hostGUIPollWait is how long a frame request waits for the picture to change before answering "nothing new".
var hostGUIPollWait = 25 * time.Second

// hostGUIFrameHandler returns a JPEG of the app. With ?after=<seq> it waits until the picture is newer than that
// (answering 204 if it is not within hostGUIPollWait), which is how the viewer follows the app without polling blind.
func (s *Server) hostGUIFrameHandler(w http.ResponseWriter, r *http.Request) {
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
	sess.Touch()
	ctx := r.Context()
	sess.Watch(ctx)
	s.markDesktopViewed(ctx, q.Get("desktop"))

	var f winhost.Frame
	if after, err := strconv.ParseUint(q.Get("after"), 10, 64); err == nil {
		waitCtx, cancel := context.WithTimeout(ctx, hostGUIPollWait)
		defer cancel()
		if f, err = sess.WaitFrame(waitCtx, after); err != nil {
			if ctx.Err() == nil && sess.Alive() {
				w.WriteHeader(http.StatusNoContent) // nothing changed yet; ask again
				return
			}
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
	} else if f, ok = sess.Latest(); !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Seq", strconv.FormatUint(f.Seq, 10))
	w.Header().Set("X-Width", strconv.Itoa(f.W))
	w.Header().Set("X-Height", strconv.Itoa(f.H))
	w.Write(f.JPEG)
}

// hostGUIInputHandler forwards one mouse or keyboard event to the app.
func (s *Server) hostGUIInputHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := readHostGUIRequest(w, r)
	if !ok {
		return
	}
	if !s.hostGUIAuthorised(req.ID, req.Token) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sess, found := s.hostGUI().Get(req.ID)
	if !found {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	var err error
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
		text, cerr := sess.Copy(req.VK)
		if cerr != nil {
			log.Printf("host-gui: copy on %s: %v", req.ID, cerr)
			http.Error(w, "copy failed", http.StatusBadGateway)
			return
		}
		jsonResponse(w, map[string]string{"text": text})
		return
	case "key":
		err = sess.Key(req.VK, req.Mods)
	default:
		http.Error(w, "unknown input kind", http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("host-gui: input %s on %s: %v", req.Kind, req.ID, err)
		http.Error(w, "input failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
