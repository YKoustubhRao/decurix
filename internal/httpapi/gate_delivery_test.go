package httpapi

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// gateDesktop is a stand-in desktop "box": a Docker daemon that reports it in state (stopped, paused or running; a
// start or unpause makes it running), whose image has the gate's user unless noGate, and one HTTP stub that serves
// both its agent's :8081 and its gate's :8083, the gate answering gateStatus.
type gateDesktop struct {
	mu         sync.Mutex
	state      string
	noGate     bool
	gateStatus int
	recording  string   // what GET /recordings/<session> answers ("" a 404)
	log        []string // "start", "unpause", "gate /request", "gate /secrets", "agent"
}

func (g *gateDesktop) record(what string) {
	g.mu.Lock()
	g.log = append(g.log, what)
	g.mu.Unlock()
}

func (g *gateDesktop) events() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.log...)
}

func newGateDesktop(t *testing.T, s *Server, state string) *gateDesktop {
	t.Helper()
	g := &gateDesktop{state: state, gateStatus: http.StatusNoContent}
	stubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if q := r.URL.RawQuery; strings.Contains(q, ":"+desktop.GatePort) {
			path := q[strings.Index(q, ":"+desktop.GatePort)+len(":"+desktop.GatePort):]
			g.record("gate " + path)
			g.mu.Lock()
			status := g.gateStatus
			g.mu.Unlock()
			w.WriteHeader(status)
			return
		}
		_ = raw
		if r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "/recordings/") {
			g.mu.Lock()
			program := g.recording
			g.mu.Unlock()
			if program == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(program))
			return
		}
		g.record("agent") // the agent's :8081, a request or a taught door's call
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(stubSrv.Close)
	stub := strings.TrimPrefix(stubSrv.URL, "http://") + "/?"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case strings.HasSuffix(p, "/containers/json"):
			_, _ = w.Write([]byte("[]"))
		case strings.HasSuffix(p, "/containers/desktop_box/json"):
			w.Header().Set("Content-Type", "application/json")
			st := &container.State{Running: g.state != "stopped", Paused: g.state == "paused", Status: g.state,
				StartedAt: "2026-11-04T10:00:00Z"}
			labels := map[string]string{}
			if g.noGate {
				labels[desktop.GateLabel] = "0"
			}
			_ = json.NewEncoder(w).Encode(container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{ID: "box", Name: "/desktop_box", State: st},
				Config:            &container.Config{Labels: labels},
				NetworkSettings:   &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{desktop.NetworkName: {IPAddress: stub}}},
			})
		case strings.HasSuffix(p, "/start"):
			g.log = append(g.log, "start")
			g.state = "running"
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/unpause"):
			g.log = append(g.log, "unpause")
			g.state = "running"
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/archive") && r.URL.Query().Get("path") == "/etc/passwd":
			users := "root:x:0:0::/root:/bin/bash\nheadless:x:1001:1001::/home/headless:/bin/bash\n"
			if !g.noGate {
				users += desktop.GateUser + ":x:999:999::/nonexistent:/usr/sbin/nologin\n"
			}
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			_ = tw.WriteHeader(&tar.Header{Name: "passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(users))})
			_, _ = tw.Write([]byte(users))
			_ = tw.Close()
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"passwd","size":1,"mode":420}`)))
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(b.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	cli, err := client.NewClientWithOpts(client.WithHTTPClient(daemon.Client()),
		client.WithHost("tcp://"+strings.TrimPrefix(daemon.URL, "http://")), client.WithVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	s.Desktops = desktop.NewManager(cli)
	s.Desktops.HostGUISecret = []byte("test secret")
	return g
}

const gatedRequest = "sign in to records.example.test, password: Plum7Orbit"

// A gate that cannot be told what was asked stops the request: nothing reaches the agent, and the send says so
// (review CP-A-2 B1, P5).
func TestAGateThatCannotBeToldStopsTheRequest(t *testing.T) {
	s := chatServer(t)
	g := newGateDesktop(t, s, "running")
	g.gateStatus = http.StatusInternalServerError
	rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {gatedRequest}}, false, "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "approval gate") {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	for _, e := range g.events() {
		if e == "agent" {
			t.Fatalf("the agent got the request though its gate was not told: %v", g.events())
		}
	}
}

// A stopped desktop is started first, and its gate is told after the start and before the agent; a paused one is
// woken first.
func TestTheGateIsToldAfterTheDesktopIsUp(t *testing.T) {
	for _, state := range []string{"stopped", "paused", "running"} {
		t.Run(state, func(t *testing.T) {
			s := chatServer(t)
			g := newGateDesktop(t, s, state)
			if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {gatedRequest}}, false, ""); rec.Code != http.StatusOK {
				t.Fatalf("send: %d %s", rec.Code, rec.Body)
			}
			got := strings.Join(g.events(), ",")
			want := "gate /secrets,gate /request,agent"
			switch state {
			case "stopped":
				want = "start," + want
			case "paused":
				want = "unpause," + want
			}
			if got != want {
				t.Fatalf("events = %s, want %s", got, want)
			}
		})
	}
}

// A desktop the control plane made without a gate (its label says so) gets its request as before, and no gate post.
func TestADesktopWithNoGateGetsItsRequest(t *testing.T) {
	s := chatServer(t)
	g := newGateDesktop(t, s, "running")
	g.noGate = true
	g.gateStatus = http.StatusInternalServerError
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {gatedRequest}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(g.events(), ","); got != "agent" {
		t.Fatalf("events = %s", got)
	}
}

// replayWith posts a replay of a recording on box with params, as a person at this machine.
func replayWith(t *testing.T, s *Server, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(params)
	form := url.Values{"id": {"box"}, "session": {"task-20261104-101500"}, "params": {string(raw)}}
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/recordings/replay", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:40000"
	rec := httptest.NewRecorder()
	s.replayRecordingHandler(rec, req)
	return rec
}

// A replay's secret parameter is told to the gate before the agent has it, with the gate's copy, and held by the
// call recorder; an ordinary parameter is neither (review CP-A-3 B1, P6). A gate that cannot be told stops the
// replay.
func TestAReplayTellsTheGateFirst(t *testing.T) {
	s := recordingServer(t)
	g := newGateDesktop(t, s, "running")
	g.recording = `{"program":{"params":[{"name":"password","secret":true},{"name":"invoice","secret":false}]}}`
	const v = "Tulip-31-Quay"
	if rec := replayWith(t, s, map[string]string{"password": v, "invoice": "INV-0042"}); rec.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(g.events(), ","); got != "gate /secrets,gate /request,agent" {
		t.Fatalf("events = %s: the gate is told first", got)
	}
	held := strings.Join(s.secretsSeen.For("box"), "|")
	if !strings.Contains(held, v) || strings.Contains(held, "INV-0042") {
		t.Fatalf("the recorder holds %q", held)
	}
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": `Running: gui type e3 "` + v + `"`}}})
	proxyCall(t, s, "azure", "box", string(body), map[string]string{"X-Decurix-Purpose": "operator.turn"})
	if hits := blobsHolding(t, s, v); len(hits) > 0 {
		t.Fatalf("the replay's password is in a blob: %s", hits[0])
	}

	s2 := recordingServer(t)
	g2 := newGateDesktop(t, s2, "running")
	g2.gateStatus = http.StatusInternalServerError
	if rec := replayWith(t, s2, map[string]string{"password": v}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a replay whose gate cannot be told: %d %s", rec.Code, rec.Body)
	}
	for _, e := range g2.events() {
		if e == "agent" || strings.HasPrefix(e, "agent") {
			t.Fatalf("the replay reached the agent: %v", g2.events())
		}
	}
}

// A replay's parameters are secret by structure: the recording's program marks them (the recorder withheld what was
// typed, or the field is a password field), whatever they are called, and an ordinary one is not held; a program
// that cannot be read holds every value given (review CP-A-4 B1).
func TestAReplaysSecretParametersAreReadFromItsProgram(t *testing.T) {
	names := []string{"confirm_password", "password_2", "login_pin", "one_time_password", "user_password"}
	params := map[string]string{"invoice_no": "INV-0042", "extra_field": "Unlisted-55"}
	var programParams []string
	for i, n := range names {
		params[n] = fmt.Sprintf("Val%d-xq-%d", i, i*7+3)
		programParams = append(programParams, fmt.Sprintf(`{"name":%q,"secret":true}`, n))
	}
	programParams = append(programParams, `{"name":"invoice_no","secret":false}`)

	s := recordingServer(t)
	g := newGateDesktop(t, s, "running")
	g.recording = `{"meta":{},"events":[],"program":{"params":[` + strings.Join(programParams, ",") + `]}}`
	if rec := replayWith(t, s, params); rec.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	held := strings.Join(s.secretsSeen.For("box"), "|")
	for _, n := range names {
		if !strings.Contains(held, params[n]) {
			t.Errorf("%s (marked secret by the program) is not held: %q", n, held)
		}
	}
	if strings.Contains(held, "INV-0042") {
		t.Errorf("an ordinary parameter is held: %q", held)
	}
	if !strings.Contains(held, "Unlisted-55") {
		t.Errorf("a parameter the program does not list is not held: %q", held)
	}

	// The program cannot be read: every value given is held.
	s2 := recordingServer(t)
	newGateDesktop(t, s2, "running")
	if rec := replayWith(t, s2, map[string]string{"invoice_no": "INV-0042", "user_password": "Val9-xq-9"}); rec.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	held = strings.Join(s2.secretsSeen.For("box"), "|")
	if !strings.Contains(held, "INV-0042") || !strings.Contains(held, "Val9-xq-9") {
		t.Fatalf("with no program, the recorder holds %q", held)
	}
}

// A desktop that may have been made before its gate is told to be recreated, not to try again.
func TestADesktopWithoutAGateIsToldToBeRecreated(t *testing.T) {
	rec := httptest.NewRecorder()
	sendRequestError(rec, fmt.Errorf("x: %w: %w", desktop.ErrGateNotTold, desktop.ErrGateMaybeMissing))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Recreate this desktop") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
