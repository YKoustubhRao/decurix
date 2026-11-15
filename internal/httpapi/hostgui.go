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
	s.hostGUIOnce.Do(func() {
		s.hostGUIMgr = winhost.NewManager(hostGUIStarter)
		s.hostGUIMgr.OnGiveUp = s.hostAppGaveUp
		s.hostGUIMgr.Reap = reapHostProcesses
	})
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
		if isForwarded(r) {
			http.Error(w, "not available through a tunnel or proxy", http.StatusForbidden)
			return
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
	// desktopName is already reduced to [A-Za-z0-9_] by winhost, and the instance tag is hex, so both are safe in a
	// shell word. The tag comes first: the sweep's pattern expects it right after the script (ownHelperMatch).
	shell := fmt.Sprintf(wslPowershellPath+bridgeOptIns(os.Getenv)+
		`powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$(wslpath -w %s)" %s %s -DesktopName %s`,
		script, instanceFlag, hostInstanceTag(), desktopName)
	// Run on the host with stdin left open for the caller: directly when the control plane is on the host itself, through
	// the same docker/nsenter hop createHostCmd uses when it is in a container - named and removed when its session ends
	// (desktop.HopCommand): a killed docker client left it running.
	if cmd := hostHop(ctx, "powershell.exe", "bridge", true, shell); cmd != nil {
		return cmd, nil
	}
	return exec.CommandContext(ctx, "bash", "-c", shell), nil
}

// bridgeOptIns are the person's opt-ins the Windows helper reads from its environment, set in the control plane's own
// (its .env), as a WSL shell's assignments that carry them across to the Windows program (WSLENV); "" when none is on.
// A Windows program started from WSL gets the Windows environment WSL captured when it started, so a variable set on
// the computer afterwards never reached the helper, and in the app's own distro the control plane's .env is the
// person's one file of settings (found 2026-11-12, asked for: bring a Chrome profile's sign-ins over). Only the exact
// value "1" is passed; the names are constants, so nothing here comes from outside into the shell.
func bridgeOptIns(getenv func(string) string) string {
	const importProfile = "DECURIX_IMPORT_HOST_BROWSER_PROFILE"
	if getenv(importProfile) != "1" {
		return ""
	}
	return importProfile + `=1 WSLENV="` + importProfile + `${WSLENV:+:$WSLENV}" `
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
	args, err := nativeHostGUICmdArgs(script, desktopName)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, ps, args...)
	// The control plane has no console when the app starts it, so PowerShell would open one of its own, on the person's
	// screen, for as long as the helper lives.
	desktop.HideWindow(cmd)
	return cmd, nil
}

// nativeHostGUICmdArgs is PowerShell's arguments for the helper at script: this instance's tag first, right after
// the script, where the sweeps look for it (ownHelperMatch).
func nativeHostGUICmdArgs(script, desktopName string) ([]string, error) {
	if desktopName == "" {
		return nil, errors.New("no desktop name for the host helper")
	}
	return []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script,
		instanceFlag, hostInstanceTag(), "-DesktopName", desktopName}, nil
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
	// Copied to a desktop that is being made or is running: kept now (an app given up on is tried again).
	s.hostGUI().Keep(desktopID, id, target, args)
	if s.Desktops != nil {
		s.Desktops.ClearAppUnavailable(desktopID, keptAppSlug(desktopID, id))
	}

	s.keptHostAppsMu.Lock()
	defer s.keptHostAppsMu.Unlock()
	if s.keptHostApps == nil {
		s.keptHostApps = make(map[string]keptHostApp)
	}
	if s.keptHostAppsLive == nil {
		s.keptHostAppsLive = make(map[string]bool)
	}
	s.keptHostApps[id] = keptHostApp{Desktop: desktopID, ID: id, Target: target, Args: args}
	s.keptHostAppsLive[desktopID] = true
	s.saveKeptHostAppsLocked()
}

// hostAppSyncInterval is how often the apps kept ready are matched to the desktops that are running: a desktop stopped
// by idle-stop, or one whose container ended on its own, lets its apps go within this long.
var hostAppSyncInterval = time.Minute

// syncHostApps keeps ready only the apps of desktops that are running (a paused desktop does not count: its apps
// are ended and kept again when it resumes). A desktop that stopped has its apps ended - on a private desktop of the host, each with its bridge, they
// cost memory nobody can use - and the set is remembered, so the desktop's next start keeps them again. Found live
// (2026-10-20): with one worker running, 15-18 bridges ran for stopped workers' apps, one relaunched every two minutes.
func (s *Server) syncHostApps(ctx context.Context) {
	if !hostGUISupported() && s.hostAppDesktops == nil {
		return
	}
	list := s.hostAppDesktops
	if list == nil {
		if s.Desktops == nil {
			return
		}
		list = s.Desktops.List
	}
	desktops, err := list(ctx)
	if err != nil {
		log.Printf("host apps: could not see which desktops are running: %v", err)
		return
	}
	running := map[string]bool{}
	for _, d := range desktops {
		// A paused desktop's apps are ended too and kept again when it resumes: an idle worker must not keep Office
		// open on the person's computer, holding their files (found live 2026-10-20: nine kept Office apps of paused
		// workers, one still holding a document of a run stopped hours before).
		if d.Status == "running" {
			running[d.ID] = true
		}
	}

	s.keptHostAppsMu.Lock()
	if s.keptHostAppsLive == nil {
		s.keptHostAppsLive = make(map[string]bool)
	}
	var keep []keptHostApp
	var release []string
	byDesktop := map[string][]keptHostApp{}
	for _, app := range s.keptHostApps {
		byDesktop[app.Desktop] = append(byDesktop[app.Desktop], app)
	}
	for desktopID, apps := range byDesktop {
		switch {
		case running[desktopID] && !s.keptHostAppsLive[desktopID]:
			keep = append(keep, apps...)
			s.keptHostAppsLive[desktopID] = true
		case !running[desktopID] && s.keptHostAppsLive[desktopID]:
			release = append(release, desktopID)
			delete(s.keptHostAppsLive, desktopID)
		}
	}
	s.keptHostAppsMu.Unlock()

	for _, desktopID := range release {
		log.Printf("host apps: %s is not running - ending the apps kept ready for it until it starts again", desktopID)
		s.hostGUI().ForgetDesktop(desktopID)
	}
	for _, app := range keep {
		if s.Desktops != nil {
			s.Desktops.ClearAppUnavailable(app.Desktop, keptAppSlug(app.Desktop, app.ID))
		}
		s.hostGUI().Keep(app.Desktop, app.ID, app.Target, app.Args)
	}
}

// releaseHostApps ends the apps kept ready for a desktop that was stopped, remembering them for its next start.
func (s *Server) releaseHostApps(desktopID string) {
	s.keptHostAppsMu.Lock()
	had := s.keptHostAppsLive[desktopID]
	delete(s.keptHostAppsLive, desktopID)
	s.keptHostAppsMu.Unlock()
	if had && (hostGUISupported() || s.hostAppDesktops != nil) {
		s.hostGUI().ForgetDesktop(desktopID)
	}
}

// keptAppSlug is the launcher slug in a kept app's session id (desktop.HostSessionSlug): "<slug>-<hash>", the slug read
// back whole for the app's name on its card (RW1-D-05). An id from before the hash ("<desktop>-<slug>") still reads.
func keptAppSlug(desktopID, id string) string {
	return desktop.HostSessionSlug(strings.TrimPrefix(id, desktopID+"-"))
}

// hostAppGaveUp is winhost's OnGiveUp: an app the host cannot keep running is recorded as unavailable on its desktop,
// with the reason, where the card and the desktop's agent see it.
func (s *Server) hostAppGaveUp(desktopID, id, reason string) {
	if s.Desktops == nil {
		return
	}
	slug := keptAppSlug(desktopID, id)
	words := strings.Fields(strings.ReplaceAll(slug, "-", " "))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	s.Desktops.MarkAppUnavailable(desktopID, slug, strings.Join(words, " "), reason)
}

// forgetHostApps ends the apps kept ready for a VM that is being deleted: nobody can open them any more, and an app on a
// private desktop is invisible to the person who would otherwise close it. Their private profiles on the host (a
// browser's sign-ins, an app's data) go with them (B-19; winhost.DeleteDesktop, in the background: it waits for the
// apps to close). A stop keeps them (releaseHostApps): they are what keeps the worker signed in.
func (s *Server) forgetHostApps(desktopID string) {
	s.keptHostAppsMu.Lock()
	delete(s.keptHostAppsLive, desktopID)
	var ids []string
	for id, app := range s.keptHostApps {
		if app.Desktop == desktopID {
			delete(s.keptHostApps, id)
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		s.saveKeptHostAppsLocked()
	}
	s.keptHostAppsMu.Unlock()
	switch {
	case s.deleteHostProfiles != nil:
		s.deleteHostProfiles(desktopID, ids)
	case hostGUISupported():
		go func() { _, _ = s.hostGUI().DeleteDesktop(desktopID, ids) }()
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
	if s.keptHostApps == nil {
		s.keptHostApps = make(map[string]keptHostApp)
	}
	for _, app := range apps {
		if !hostGUIID.MatchString(app.ID) || (listed && !exists[app.Desktop]) {
			continue
		}
		// A browser an older build kept ready runs in place now, and is never started ahead: it would hold the person's profile
		// from the moment the control plane starts, and their own Chrome could not be opened (found live 2026-11-12).
		if desktop.SlugRunsInPlace(keptAppSlug(app.Desktop, app.ID)) {
			continue
		}
		s.keptHostApps[app.ID] = app
	}
	s.saveKeptHostAppsLocked() // drops the apps of VMs deleted while the control plane was down
	s.keptHostAppsMu.Unlock()

	// Kept ready only for the desktops that are running, now and from here on.
	s.syncHostApps(ctx)
	go s.watchHostApps(ctx)
}

// watchHostApps runs syncHostApps every hostAppSyncInterval until ctx ends.
func (s *Server) watchHostApps(ctx context.Context) {
	t := time.NewTicker(hostAppSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncHostApps(ctx)
		}
	}
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
	// Append is for setvalue: add the text to what the field holds instead of replacing it.
	Append bool `json:"append,omitempty"`
}

func readHostGUIRequest(w http.ResponseWriter, r *http.Request) (hostGUIRequest, bool) {
	var req hostGUIRequest
	if !allowMethod(w, r, http.MethodPost) {
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
		jsonResponse(w, map[string]string{"error": viewerReason(err)})
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
// Viewer to server, text JSON, one input each (see applyInput); a copy carries an "id" to be answered under. A
// {"kind":"controls"} is answered {"id":n,"controls":{...}}: the app's controls to lay over the picture (hostcontrols.go).
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
	s.serveSessionSocket(w, r, sess, q.Get("id"), q.Get("desktop"), nil)
}

// serveSessionSocket is one viewer's WebSocket to a private desktop, for as long as either end keeps it open: the
// protocol hostGUIWSHandler describes. label names the session in the log; desktopID is the desktop to keep marked as
// in use meanwhile ("" for none). before, when given, is called with each input before it is applied (a native
// worker's demonstration recorder, nativerecording.go).
func (s *Server) serveSessionSocket(w http.ResponseWriter, r *http.Request, sess *winhost.Session, label, desktopID string, before func(hostGUIRequest)) {
	c, err := wsconn.Upgrade(w, r)
	if err != nil {
		return
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.markDesktopViewed(ctx, desktopID)
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
		if req.Kind == "controls" {
			// Answered from the last read (a new one is started), so the read loop does not wait on it.
			out, _ := json.Marshal(map[string]any{"id": req.Seq, "controls": s.sessionControls(sess, sess.ID())})
			if c.Write(wsconn.OpText, out) != nil {
				return
			}
			continue
		}
		if before != nil {
			before(req)
		}
		s.noteHostInput(sess.ID())
		text, wantsText, err := applyInput(sess, req)
		if err != nil {
			log.Printf("host-gui: input %s on %s: %v", req.Kind, label, err)
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
	case "press":
		text, err = sess.Press(req.X, req.Y)
		if err != nil {
			return "ERR " + viewerReason(err), true, nil
		}
		return text, true, nil
	case "setfocus":
		err = sess.SetFocus(req.X, req.Y)
	case "setvalue":
		// The answer is what the field holds afterwards, or why it could not be set (the viewer then types the text).
		text, err = sess.SetValue(req.X, req.Y, req.Text, req.Append)
		if err != nil {
			return "ERR " + viewerReason(err), true, nil
		}
		return "SET " + text, true, nil
	case "choose":
		// The answer is the entry chosen, or why none was: a drop-down's entries are not shown to the viewer.
		text, err = sess.Choose(req.X, req.Y, req.Text)
		if err != nil {
			return "ERR " + viewerReason(err), true, nil
		}
		return text, true, nil
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
