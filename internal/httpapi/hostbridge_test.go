package httpapi

import (
	"archive/tar"
	"encoding/json"
	"github.com/YKoustubhRao/decurix/internal/winhost"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

func TestDetectHostAppsHandler(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/host-apps/detect", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var items []desktop.HostAppItem
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}

	if len(items) == 0 {
		t.Fatalf("expected non-empty host app list")
	}

	foundBridge := false
	for _, it := range items {
		if it.ID == "host_bridge" {
			foundBridge = true
			if !it.Detected {
				t.Errorf("host_bridge should be detected")
			}
		}
	}
	if !foundBridge {
		t.Errorf("expected host_bridge in /api/host-apps/detect response")
	}

	// Verify ?refresh=1 force refresh parameter
	reqRef := httptest.NewRequest(http.MethodGet, "/api/host-apps/detect?refresh=1", nil)
	recRef := httptest.NewRecorder()
	mux.ServeHTTP(recRef, reqRef)
	if recRef.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on refresh, got %d: %s", recRef.Code, recRef.Body.String())
	}
}

func TestHostBridgeExecHandlerCLI(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterLocalRoutes(mux)

	// Windows has no echo program (it is a cmd.exe built-in, and cmd.exe is run detached as a GUI launcher), so there
	// the check is that a plain program's output comes back.
	cmd, args, want := "echo", []string{"hello-from-host-bridge"}, "hello-from-host-bridge"
	if runtime.GOOS == "windows" {
		cmd, args, want = "hostname", nil, ""
	}
	payload := signedExecPayload(srv, "vm1", cmd, args...)
	req := httptest.NewRequest(http.MethodPost, "/api/host-bridge/exec", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp hostBridgeExecResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Success {
		t.Errorf("expected success=true, got error=%s", resp.Error)
	}
	if !strings.Contains(resp.Stdout, want) || strings.TrimSpace(resp.Stdout) == "" {
		t.Errorf("expected output to contain %q, got %q", want, resp.Stdout)
	}
}

func TestHostBridgeExecHandlerValidation(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterLocalRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/host-bridge/exec", strings.NewReader(`{"cmd": ""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty cmd, got %d", rec.Code)
	}
}

func TestHostBridgeOpenHandlerValidation(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterLocalRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/host-bridge/open", strings.NewReader(`{"target": ""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty target, got %d", rec.Code)
	}
}

func TestFindDockerBinary(t *testing.T) {
	_ = findDockerBinary()
}

func TestCreateHostCmdWindowsCommand(t *testing.T) {
	cmd := createHostCmd(nil, "172.18.0.2:1", "cmd.exe", "/c", "start", "explorer.exe", "shell:AppsFolder\\OpenAI.Codex_2p2nqsd0c76g0!App")
	if cmd == nil {
		t.Fatal("expected non-nil cmd")
	}
	if len(cmd.Args) == 0 {
		t.Fatal("expected cmd to have args")
	}
}

// archiveDocker is a stand-in Docker daemon that reports one desktop and keeps the tar it is asked to extract into it.
func archiveDocker(t *testing.T, desktopID string) (*desktop.Manager, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	files := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/desktop_"+desktopID+"/json"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: desktopID}})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/containers/"+desktopID+"/archive"):
			tr := tar.NewReader(r.Body)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(tr)
				mu.Lock()
				files[hdr.Name] = string(data)
				mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHTTPClient(ts.Client()), client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")), client.WithVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	return desktop.NewManager(cli), func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for k, v := range files {
			out[k] = v
		}
		return out
	}
}

func TestCopyHostAppsIntoAnExistingDesktop(t *testing.T) {
	mgr, written := archiveDocker(t, "work")
	srv := &Server{Desktops: mgr}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	post := func(form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/host-apps/copy", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("id=work&host_apps=host_bridge"); rec.Code != http.StatusOK {
		t.Fatalf("copy = %d: %s", rec.Code, rec.Body.String())
	}
	hostRun, ok := written()[".local/bin/host-run"]
	if !ok {
		t.Fatalf("the desktop was not sent host-run; it got %v", written())
	}
	if strings.Contains(hostRun, "curl -s") {
		t.Fatal("the refreshed host-run still calls curl")
	}

	if rec := post("id=nope&host_apps=host_bridge"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown desktop = %d, want 404", rec.Code)
	}
	if rec := post("id=work"); rec.Code != http.StatusBadRequest {
		t.Errorf("no apps = %d, want 400", rec.Code)
	}
	if rec := post("host_apps=host_bridge"); rec.Code != http.StatusBadRequest {
		t.Errorf("no id = %d, want 400", rec.Code)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/host-apps/copy", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
}

func TestTheHostIsWarmedWhileSomeoneChoosesAppsAndWhileTheyAreCopied(t *testing.T) {
	var warmed int32
	desktop.SetHostAppWarmer(func() { atomic.AddInt32(&warmed, 1) })
	defer desktop.SetHostAppWarmer(nil)

	mgr, _ := archiveDocker(t, "work")
	srv := &Server{Desktops: mgr}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Opening the list of apps to copy means a desktop is about to be created: the helper should be started meanwhile.
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/host-apps/detect", nil))
	if atomic.LoadInt32(&warmed) == 0 {
		t.Error("listing the host apps did not warm the host")
	}

	// And so does copying them, which is what happens while the desktop itself is still starting.
	before := atomic.LoadInt32(&warmed)
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/host-apps/copy", strings.NewReader("id=work&host_apps=host_bridge"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if atomic.LoadInt32(&warmed) == before {
		t.Error("copying host apps did not warm the host")
	}
}

// The gated (LAN/tunnel) listener is built from RegisterRoutes alone, so
// nothing that runs a command on the host or shows a host program may be
// registered there - holding the access code must not be host code execution.
func TestHostRoutesAreOnlyOnThePrimaryListener(t *testing.T) {
	hostRoutes := []string{
		"/api/host-bridge/exec", "/api/host-bridge/open", "/api/host-bridge/clip",
		"/api/host-gui/start", "/api/host-gui/ws", "/api/host-gui/stop", "/host-gui/view",
		// The access code and the paired-device list are never served over
		// the listener they gate.
		"/api/lan-access/status", "/api/lan-access/rotate",
		"/api/lan-access/devices", "/api/lan-access/devices/revoke",
		"/api/cftunnel", "/api/cftunnel/configure", "/api/cftunnel/disable",
		// A phone app is shown only in a VM's window, like a host app.
		"/phone-app/view", "/api/phone-app/ws",
	}
	srv := &Server{}
	gated := http.NewServeMux()
	srv.RegisterRoutes(gated)
	primary := http.NewServeMux()
	srv.RegisterRoutes(primary)
	srv.RegisterLocalRoutes(primary)

	for _, path := range hostRoutes {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, pattern := gated.Handler(req); pattern == path {
			t.Errorf("%s is registered on the gated listener", path)
		}
		if _, pattern := primary.Handler(req); pattern != path {
			t.Errorf("%s is missing from the primary listener (matched %q)", path, pattern)
		}
	}
}

// signedExecPayload is what a launcher's host-run sends: the command with the token it was written with.
func signedExecPayload(srv *Server, desktop, cmd string, args ...string) string {
	b, _ := json.Marshal(hostBridgeExecRequest{
		Cmd: cmd, Args: args, Desktop: desktop,
		Token: winhost.ExecToken(srv.hostGUISecretKey(), desktop, cmd, args),
	})
	return string(b)
}

func execStatus(t *testing.T, srv *Server, body string) int {
	t.Helper()
	mux := http.NewServeMux()
	srv.RegisterLocalRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/host-bridge/exec", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

// A desktop's agent can call this endpoint over the Docker network, so nothing but a command a launcher was signed
// for may run - not an unsigned one, not a signed one with an argument added, not another desktop's.
func TestHostBridgeExecRefusesAnythingNotSigned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs touch, which Windows does not have")
	}
	srv := &Server{}
	marker := filepath.Join(t.TempDir(), "ran")

	signed := signedExecPayload(srv, "vm1", "touch", marker)
	var good hostBridgeExecRequest
	_ = json.Unmarshal([]byte(signed), &good)

	altered := good
	altered.Args = append(append([]string{}, good.Args...), "extra")
	otherDesktop := good
	otherDesktop.Desktop = "vm2"
	unsigned := good
	unsigned.Token = ""
	guiToken := good
	guiToken.Token = winhost.Token(srv.hostGUISecretKey(), "vm1", "touch", "")

	for name, req := range map[string]hostBridgeExecRequest{
		"unsigned": unsigned, "an added argument": altered, "another desktop": otherDesktop, "a host-gui launch token": guiToken,
	} {
		b, _ := json.Marshal(req)
		if code := execStatus(t, srv, string(b)); code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, code)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused command ran anyway")
	}
	if code := execStatus(t, srv, signed); code != http.StatusOK {
		t.Fatalf("the signed command: got %d, want 200", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the signed command did not run: %v", err)
	}
}

func TestSafeOpenURL(t *testing.T) {
	for _, ok := range []string{"https://claude.ai", "http://localhost:3000/a?b=1&c=2#x", "https://x.com/a%20b"} {
		if !safeOpenURL(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{
		"", "file:///etc/passwd", "C:\\Windows\\System32\\calc.exe", "/usr/bin/xterm", "javascript:alert(1)",
		"https://x.com/$(id)", "https://x.com/`id`", "https://x.com/\"; calc", "https://x.com/ a", "https://x.com/a\nb",
		"https://x.com/a|b", "https://x.com/a;b", "https://x.com/a^b", "https://x.com/<a>", "https://x.com/'a",
		"https://x.com/" + strings.Repeat("a", 3000),
	} {
		if safeOpenURL(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHostBridgeOpenAndClipLimits(t *testing.T) {
	srv := &Server{}
	mux := http.NewServeMux()
	srv.RegisterLocalRoutes(mux)
	post := func(path, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("/api/host-bridge/open", `{"target":"C:\\Windows\\System32\\calc.exe"}`); code != http.StatusBadRequest {
		t.Errorf("open of a program path: got %d, want 400", code)
	}
	big := `{"text":"` + strings.Repeat("a", maxHostClipBytes+10) + `"}`
	if code := post("/api/host-bridge/clip", big); code != http.StatusBadRequest {
		t.Errorf("an oversized clipboard write: got %d, want 400", code)
	}
}

// An opener that fails must be reported as a failure: a control plane in a container has no xdg-open, and it used to
// answer success for a link that was never opened.
func TestStartAndWatchReportsAnOpenerThatFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses bash")
	}
	if err := startAndWatch(exec.Command("bash", "-c", "echo no opener here >&2; exit 3"), 5*time.Second); err == nil ||
		!strings.Contains(err.Error(), "no opener here") {
		t.Fatalf("a failing opener gave %v, want its error with what it printed", err)
	}
	if err := startAndWatch(exec.Command("bash", "-c", "exit 0"), 5*time.Second); err != nil {
		t.Fatalf("an opener that worked gave %v", err)
	}
	// One still running after the wait is a browser that stays attached, not a failure.
	if err := startAndWatch(exec.Command("bash", "-c", "sleep 5"), 100*time.Millisecond); err != nil {
		t.Fatalf("an opener still running gave %v", err)
	}
	if err := startAndWatch(exec.Command("/no/such/opener"), time.Second); err == nil {
		t.Fatal("an opener that could not be started was reported as working")
	}
}
