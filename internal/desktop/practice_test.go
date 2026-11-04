package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// practiceDaemon is a stand-in Docker daemon for the practice adapter: it
// records every request and answers the few the practice code makes.
type practiceDaemon struct {
	t               *testing.T
	mu              sync.Mutex
	calls           []string
	created         []map[string]any
	copiedTo        map[string][]map[string]string // "<container>:<path>" -> tars' files
	copiedRaw       map[string][][]byte            // "<container>:<path>" -> the tars as sent
	copiedUIDGID    map[string][]string            // "<container>:<path>" -> each copy's copyUIDGID
	paused          map[string]bool
	readWhilePaused bool
	netExists       bool
	caseTar         map[string]string
	image           string
	execs           map[string][]string // exec id -> container, kind ("tarin"/"tarout"/"run"), path
	noGateUser      bool                // the image is one from before the approval gate
}

func tarFiles(b []byte) map[string]string {
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			return out
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body)
	}
}

func tarBody(files map[string]string, dirs ...string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, d := range dirs {
		_ = tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755})
	}
	for name, text := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(text))})
		_, _ = tw.Write([]byte(text))
	}
	_ = tw.Close()
	return b.Bytes()
}

func (d *practiceDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := r.URL.Path
	if i := strings.Index(p[1:], "/"); strings.HasPrefix(p, "/v1.") && i > 0 {
		p = p[i+1:]
	}
	d.calls = append(d.calls, r.Method+" "+p)
	w.Header().Set("Content-Type", "application/json")
	notFound := func() {
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
	}
	switch {
	case p == "/info":
		_ = json.NewEncoder(w).Encode(map[string]any{"SecurityOptions": []string{"name=seccomp,profile=default"}})
	case p == "/networks/"+PracticeNetworkName && r.Method == "GET":
		if !d.netExists {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Name": PracticeNetworkName, "Internal": true, "IPAM": map[string]any{"Config": []map[string]string{{"Subnet": "172.30.0.0/16", "Gateway": "172.30.0.1"}}}})
	case p == "/networks/create":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["Internal"] != true || body["Name"] != PracticeNetworkName {
			d.t.Errorf("practice network not internal: %v", body)
		}
		d.netExists = true
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": "net1"})
	case p == "/networks" && r.Method == "GET":
		_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "net1", "Name": PracticeNetworkName}})
	case strings.HasPrefix(p, "/networks/") && r.Method == "DELETE":
		w.WriteHeader(204)
	case strings.HasPrefix(p, "/images/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": d.image, "Os": "linux", "Config": map[string]any{"Env": []string{"API_KEY=hidden", "OLLAMA_URL=http://somewhere", "STARTUPDIR=/dockerstartup"}}})
	case p == "/containers/create":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.created = append(d.created, body)
		id := "desk"
		if labels, _ := body["Labels"].(map[string]any); labels["decurix-practice-mock"] == "true" {
			id = "mock"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	case strings.HasSuffix(p, "/archive") && r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		key := strings.Split(p, "/")[2] + ":" + r.URL.Query().Get("path")
		if d.copiedTo == nil {
			d.copiedTo = map[string][]map[string]string{}
		}
		d.copiedTo[key] = append(d.copiedTo[key], tarFiles(b))
		if d.copiedRaw == nil {
			d.copiedRaw, d.copiedUIDGID = map[string][][]byte{}, map[string][]string{}
		}
		d.copiedRaw[key] = append(d.copiedRaw[key], b)
		d.copiedUIDGID[key] = append(d.copiedUIDGID[key], r.URL.Query().Get("copyUIDGID"))
		w.WriteHeader(200)
	case strings.HasSuffix(p, "/archive") && r.Method == "GET":
		id, at := strings.Split(p, "/")[2], r.URL.Query().Get("path")
		if d.paused[id] {
			d.readWhilePaused = true
		}
		var body []byte
		switch {
		case at == "/opt/decurix-chromium.init":
			body = tarBody(map[string]string{"decurix-chromium.init": "CHROMIUM_FLAGS=\"--x\"\n"})
		case at == "/etc/passwd":
			users := "root:x:0:0::/root:/bin/bash\nheadless:x:1001:1001::/home/headless:/bin/bash\n"
			if !d.noGateUser {
				users += "decurixgate:x:999:999::/nonexistent:/usr/sbin/nologin\n"
			}
			body = tarBody(map[string]string{"passwd": users})
		case strings.HasPrefix(at, PracticeWorkDir+"/"):
			c := strings.TrimPrefix(at, PracticeWorkDir+"/")
			body = tarBody(map[string]string{c + "/in.csv": d.caseTar["in.csv"], c + "/out.txt": d.caseTar["out.txt"]}, c+"/")
		default:
			notFound()
			return
		}
		w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"x","size":0,"mode":420,"mtime":"2026-10-19T00:00:00Z","linkTarget":""}`)))
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(body)
	case strings.HasSuffix(p, "/start") && strings.HasPrefix(p, "/containers/"):
		w.WriteHeader(204)
	case strings.HasSuffix(p, "/stop"):
		w.WriteHeader(204)
	case strings.HasSuffix(p, "/json") && strings.HasPrefix(p, "/containers/") && p != "/containers/json":
		ctr := strings.Split(p, "/")[2]
		labels := map[string]string{PracticeLabel: "true"}
		switch ctr {
		case "worker":
			labels = map[string]string{}
		case "mock":
			labels["decurix-practice-mock"] = "true"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": ctr, "Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": true, "Paused": d.paused[ctr]}, "NetworkSettings": map[string]any{"Networks": map[string]any{PracticeNetworkName: map[string]string{"IPAddress": "172.30.0.5"}}}})
	case p == "/containers/json":
		_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "left-behind"}})
	case strings.HasSuffix(p, "/exec") && strings.HasPrefix(p, "/containers/"):
		var body struct {
			User string
			Cmd  []string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ctr := strings.Split(p, "/")[2]
		joined := strings.Join(body.Cmd, " ")
		info := []string{ctr, "run", strings.Join(body.Cmd, "\x00")}
		switch {
		case strings.Contains(joined, "tar -x -C "):
			dir := ""
			for i, a := range body.Cmd {
				if a == "-C" && i+1 < len(body.Cmd) {
					dir = body.Cmd[i+1]
				}
			}
			info = []string{ctr, "tarin", dir}
		case strings.Contains(joined, "kill -s STOP"):
			info = []string{ctr, "tarout", body.Cmd[len(body.Cmd)-3]}
		}
		if info[1] == "run" && body.User != "headless" && !(body.User == "root" && strings.Contains(joined, "/var/lib/decurix-gate/computer")) {
			d.t.Errorf("reset ran as %v: %s", body.User, joined)
		}
		if info[1] != "run" && body.User != "1001:1001" {
			d.t.Errorf("tar ran as %v", body.User)
		}
		if d.execs == nil {
			d.execs = map[string][]string{}
		}
		id := fmt.Sprintf("ex%d", len(d.execs)+1)
		d.execs[id] = info
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/start"):
		info := d.execs[strings.Split(p, "/")[2]]
		_, _ = io.Copy(io.Discard, r.Body)
		if info == nil || info[1] == "run" {
			w.WriteHeader(200)
			return
		}
		conn, buff, err := w.(http.Hijacker).Hijack()
		if err != nil {
			d.t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buff.WriteString("HTTP/1.1 101 UPGRADED" + crlf + "Content-Type: application/vnd.docker.raw-stream" + crlf + "Connection: Upgrade" + crlf + "Upgrade: tcp" + crlf + crlf)
		_ = buff.Flush()
		if info[1] == "tarin" {
			b, _ := io.ReadAll(buff)
			key := info[0] + ":" + info[2]
			if d.copiedTo == nil {
				d.copiedTo = map[string][]map[string]string{}
			}
			d.copiedTo[key] = append(d.copiedTo[key], tarFiles(b))
			return
		}
		if d.paused[info[0]] {
			d.t.Errorf("exec read from paused container %s", info[0])
		}
		if info[2] == "/work/state.json" {
			_, _ = stdcopy.NewStdWriter(conn, stdcopy.Stdout).Write(tarBody(map[string]string{"state.json": `{"orders":[{"n":"1"}]}`}))
		}
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
		_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": 0})
	case strings.HasSuffix(p, "/pause"):
		if d.paused == nil {
			d.paused = map[string]bool{}
		}
		d.paused[strings.Split(p, "/")[2]] = true
		w.WriteHeader(204)
	case strings.HasSuffix(p, "/unpause"):
		d.paused[strings.Split(p, "/")[2]] = false
		w.WriteHeader(204)
	case strings.HasPrefix(p, "/containers/") && r.Method == "DELETE":
		w.WriteHeader(204)
	default:
		d.t.Errorf("unexpected Docker request: %s %s", r.Method, p)
		notFound()
	}
}

const crlf = "\r\n"

func practiceManager(t *testing.T) (*Manager, *practiceDaemon) {
	t.Helper()
	d := &practiceDaemon{t: t, image: "sha256:" + strings.Repeat("b", 64)}
	ts := httptest.NewServer(d)
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")), client.WithVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	m := NewManager(cli)
	m.HostGUISecret = []byte("practice-secret")
	return m, d
}

// A practice desktop gets its agent token like any desktop, and without the control plane's secret it is not made at
// all - it used to be made with no token, and every request the run sent it was refused with no reason given.
func TestCreatePracticeNeedsTheSecret(t *testing.T) {
	m, d := practiceManager(t)
	spec := PracticeSpec{RunID: "x1", Image: d.image, Harness: "thebot", Brain: BrainAzure, Model: "m", BrokerURL: "http://172.30.0.1:8091"}
	if _, err := m.CreatePractice(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	var env []string
	hasToken := false
	for _, e := range d.created[0]["Env"].([]any) {
		env = append(env, e.(string))
		hasToken = hasToken || e.(string) == AgentTokenEnv+"="+m.AgentToken("practice-x1")
	}
	if !hasToken || m.AgentToken("practice-x1") == "" {
		t.Fatalf("the practice desktop has no agent token of its own: %v", env)
	}
	m.HostGUISecret = nil
	if _, err := m.CreatePractice(context.Background(), PracticeSpec{RunID: "x2", Image: d.image, BrokerURL: "http://172.30.0.1:8091"}); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("a practice desktop without the secret: %v, want ErrNoSecret", err)
	}
	if len(d.created) != 1 {
		t.Fatal("a practice desktop was made without the secret")
	}
}

func TestCreatePracticeIsIsolated(t *testing.T) {
	m, d := practiceManager(t)
	spec := PracticeSpec{RunID: "x1", Image: d.image, Harness: "thebot", Brain: BrainAzure, Model: "m", MemoryMB: 8192,
		BrokerURL: "http://172.30.0.1:8091", Hosts: []string{"app.example.test"},
		AgentTree: tarBody(map[string]string{"decurix_agent/app.py": "x"}), Snapshot: tarBody(map[string]string{"skills/a.json": "{}"})}
	got, err := m.CreatePractice(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.IP != "172.30.0.5" || got.Name != "practice-x1" || got.AgentURL() != "http://172.30.0.5:8081" {
		t.Fatalf("wrong desktop %+v", got)
	}
	if len(d.created) != 1 {
		t.Fatal("expected one container")
	}
	body := d.created[0]
	host := body["HostConfig"].(map[string]any)
	if host["NetworkMode"] != PracticeNetworkName || host["Binds"] != nil || host["ExtraHosts"] != nil || host["PortBindings"] != nil || host["Mounts"] != nil {
		t.Fatalf("practice desktop reaches beyond its network: %v", host)
	}
	if host["Memory"].(float64) != 2048*1024*1024 || host["PidsLimit"].(float64) != 512 {
		t.Fatal("resources not capped")
	}
	opts, _ := json.Marshal(host["SecurityOpt"])
	if !strings.Contains(string(opts), "seccomp=") || !strings.Contains(string(opts), "no-new-privileges:true") {
		t.Fatalf("security options lost: %s", opts)
	}
	if body["Labels"].(map[string]any)[PracticeLabel] != "true" {
		t.Fatal("practice label missing")
	}
	env := strings.Join(func() []string {
		var out []string
		for _, e := range body["Env"].([]any) {
			out = append(out, e.(string))
		}
		return out
	}(), "\n")
	for _, want := range []string{"API_KEY=\n", "OLLAMA_URL=\n", "DECURIX_PRACTICE=1", "CONTROL_PLANE_URL=http://172.30.0.1:8091", "OPENAI_BASE_URL=http://172.30.0.1:8091/api/llm/azure", "DESKTOP_ID=practice-x1"} {
		if !strings.Contains(env+"\n", want) {
			t.Errorf("env lacks %q", want)
		}
	}
	if strings.Contains(env, "hidden") || strings.Contains(env, "somewhere") || strings.Contains(env, "host.docker.internal") {
		t.Fatal("inherited secret or host address leaked into a practice desktop")
	}
	flags := d.copiedTo["desk:/opt"]
	if len(flags) != 2 || !strings.Contains(flags[0]["decurix-chromium.init"], "set -- '--host-resolver-rules=MAP app.example.test practice-mock:8000' \"$@\"") {
		t.Fatalf("browser mapping not installed: %v", flags)
	}
	if flags[1]["decurix_agent/app.py"] != "x" {
		t.Fatal("agent tree not copied")
	}
	// A never-started desktop has no .decurix folder: the state goes into the
	// home folder under .decurix/, which the tar itself creates.
	if d.copiedTo["desk:/home/headless"][0][".decurix/skills/a.json"] != "{}" {
		t.Fatalf("snapshot not restored under .decurix: %v", d.copiedTo)
	}
}

// TestPracticeDesktopsHaveTheGate checks a practice desktop is told it must have an approval gate, gets its own key
// before its first start, and is not made from an image without the gate (round-4 recheck).
func TestPracticeDesktopsHaveTheGate(t *testing.T) {
	m, d := practiceManager(t)
	spec := PracticeSpec{RunID: "x1", Image: d.image, Harness: "thebot", Brain: BrainAzure, Model: "m", BrokerURL: "http://172.30.0.1:8091"}
	if _, err := m.CreatePractice(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range d.created[0]["Env"].([]any) {
		found = found || e.(string) == GateRequiredEnv+"=1"
	}
	if !found {
		t.Fatal("a practice desktop is not told it must have a gate")
	}
	keys := d.copiedTo["desk:/"]
	if len(keys) != 1 || keys[0]["etc/decurix-gate/key"] != m.GateKey("practice-x1") || m.GateKey("practice-x1") == "" {
		t.Fatalf("the practice desktop's key was not put in place: %v", keys)
	}

	m2, d2 := practiceManager(t)
	d2.noGateUser = true
	if _, err := m2.CreatePractice(context.Background(), spec); !errors.Is(err, ErrNoGate) {
		t.Fatalf("an image without the gate: %v, want ErrNoGate", err)
	}
}

func TestCreatePracticeRefusesBadHostsAndIDs(t *testing.T) {
	m, d := practiceManager(t)
	for _, spec := range []PracticeSpec{
		{RunID: "x1", Image: d.image, BrokerURL: "http://b", Hosts: []string{"evil' ; rm -rf /"}},
		{RunID: "../x", Image: d.image, BrokerURL: "http://b"},
		{RunID: "x1", Image: "worker:latest", BrokerURL: "http://b"},
	} {
		if _, err := m.CreatePractice(context.Background(), spec); err == nil {
			t.Fatalf("accepted %+v", spec)
		}
	}
}

func TestPracticeMockAndCollection(t *testing.T) {
	m, d := practiceManager(t)
	ctx := context.Background()
	mock, err := m.StartPracticeMock(ctx, d.image, "x1", []byte("print(1)"), []byte(`{"rows":[]}`))
	if err != nil || mock != "mock" {
		t.Fatal(err)
	}
	cfg := d.created[0]
	host := cfg["HostConfig"].(map[string]any)
	if host["NetworkMode"] != PracticeNetworkName || host["ReadonlyRootfs"] != true || host["PortBindings"] != nil || host["Binds"] != nil {
		t.Fatalf("mock boundary lost: %v", host)
	}
	aliases, _ := json.Marshal(cfg["NetworkingConfig"])
	if !strings.Contains(string(aliases), PracticeMockAlias) {
		t.Fatal("mock alias missing")
	}
	files := d.copiedTo["mock:/work"][0]
	if files["mock_app.py"] != "print(1)" || files["fixture.json"] != `{"rows":[]}` {
		t.Fatal("mock files not copied")
	}
	if err := m.CopyPracticeInputs(ctx, "desk", "case-1", map[string][]byte{"in.csv": []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if d.copiedTo["desk:/home/headless"][0]["work/practice/case-1/in.csv"] != "a" {
		t.Fatal("inputs not copied into the case folder")
	}
	if err := m.PausePractice(ctx, "desk", "mock"); err != nil {
		t.Fatal(err)
	}
	d.caseTar = map[string]string{"in.csv": "changed", "out.txt": "result"}
	out, changed, problems, err := m.CollectPractice(ctx, "desk", "case-1", map[string][]byte{"in.csv": []byte("a")})
	if err != nil || len(problems) != 0 {
		t.Fatal(err, problems)
	}
	if string(out["out.txt"]) != "result" || len(changed) != 1 || changed[0] != "in.csv" {
		t.Fatalf("collection wrong: %v %v", out, changed)
	}
	state, err := m.PracticeMockState(ctx, "mock")
	if err != nil || !strings.Contains(string(state), "orders") {
		t.Fatal(err)
	}
	if !d.readWhilePaused {
		t.Fatal("evidence read before pausing")
	}
	if !d.paused["mock"] {
		t.Fatal("the mock was left running after its state was read")
	}
	if err := m.UnpausePractice(ctx, "desk", "mock"); err != nil {
		t.Fatal(err)
	}
}

func TestResetAndCleanupPractice(t *testing.T) {
	m, d := practiceManager(t)
	ctx := context.Background()
	if err := m.ResetPractice(ctx, "desk", tarBody(map[string]string{"skills/a.json": "{}"})); err != nil {
		t.Fatal(err)
	}
	// What the reset does to a real desktop is TestPracticeResetLive's to prove; here, what it is given to clear.
	var reset []string
	for _, info := range d.execs {
		if info[1] == "run" && !strings.Contains(info[2], "decurix-gate") {
			reset = strings.Split(info[2], "\x00")
		}
	}
	cleared := map[string]bool{}
	for _, p := range reset {
		cleared[p] = true
	}
	// All of the agent's and soul's state goes, whatever folders they keep in it (review EVAL-1, B1).
	for _, p := range []string{"/home/headless/.decurix", "/home/headless/work", "/home/headless/.config/chromium"} {
		if !cleared[p] {
			t.Fatalf("the reset does not clear %s: %q", p, reset)
		}
	}
	joined := strings.Join(d.calls, "\n")
	stop, restore, start := strings.Index(joined, "/containers/desk/stop"), strings.Index(joined, "PUT /containers/desk/archive"), strings.LastIndex(joined, "/containers/desk/start")
	if stop < 0 || restore < stop || start < restore {
		t.Fatalf("snapshot not restored while stopped: %s", joined)
	}
	// The restored state is the worker's: every entry owned by its uid, and copied without copyUIDGID, which would
	// chown it all to the container's configured user, root (LIVE-2 round 3, F-R3-2).
	key := "desk:/home/headless"
	if len(d.copiedRaw[key]) != 1 || d.copiedUIDGID[key][0] == "true" || d.copiedUIDGID[key][0] == "1" {
		t.Fatalf("snapshot copies %d, copyUIDGID %q", len(d.copiedRaw[key]), d.copiedUIDGID[key])
	}
	tr := tar.NewReader(bytes.NewReader(d.copiedRaw[key][0]))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Uid != 1001 || h.Gid != 1001 {
			t.Fatalf("%s is owned by %d:%d, not the worker", h.Name, h.Uid, h.Gid)
		}
	}
	d.calls = nil
	if err := m.CleanupPractice(ctx); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(d.calls, "\n")
	if !strings.Contains(joined, "DELETE /containers/left-behind") || !strings.Contains(joined, "DELETE /networks/net1") {
		t.Fatalf("cleanup incomplete: %s", joined)
	}
}

// Every place the agent and soul keep state is under the folder the reset clears whole: the agent's DECURIX_DIR
// default (config.py) and soul's WORKSPACE_ROOT (the start script).
// ResetPractice deletes a worker's whole state, so it refuses any container the control plane did not label as a
// practice desktop: a worker's own desktop, or a mock (review EVAL-2, N1).
func TestResetPracticeRefusesWhatIsNotAPracticeDesktop(t *testing.T) {
	m, d := practiceManager(t)
	for _, id := range []string{"worker", "mock"} {
		d.calls = nil
		if err := m.ResetPractice(context.Background(), id, nil); !errors.Is(err, ErrNotPractice) {
			t.Fatalf("%s: %v", id, err)
		}
		for _, c := range d.calls {
			if strings.Contains(c, "/exec") || strings.Contains(c, "/stop") {
				t.Fatalf("%s: the reset touched it: %v", id, d.calls)
			}
		}
	}
}

func TestThePracticeResetCoversEveryStateRoot(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if !strings.Contains(read("agent/decurix_agent/config.py"), `DECURIX_DIR = Path(os.environ.get("DECURIX_DIR", _default_base / ".decurix"))`) {
		t.Fatal("the agent's state folder moved; the practice reset must follow it")
	}
	m := regexp.MustCompile(`export WORKSPACE_ROOT="([^"]+)"`).FindStringSubmatch(read("agent/start_agent_terminal.sh"))
	if m == nil || !strings.HasPrefix(m[1], practiceDecurixDir+"/") {
		t.Fatalf("soul's workspace is not under %s: %v", practiceDecurixDir, m)
	}
	found := false
	for _, p := range practiceResetPaths {
		found = found || p == practiceDecurixDir
	}
	if !found {
		t.Fatal("the reset does not clear the agent's whole state folder")
	}
}

func TestTarFingerprintIgnoresOrderAndBytecode(t *testing.T) {
	a := tarBody(map[string]string{"a/x.py": "1", "a/y.py": "2"})
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range [][2]string{{"a/y.py", "2"}, {"a/__pycache__/x.pyc", "junk"}, {"a/x.py", "1"}} {
		_ = tw.WriteHeader(&tar.Header{Name: f[0], Typeflag: tar.TypeReg, Size: int64(len(f[1]))})
		_, _ = tw.Write([]byte(f[1]))
	}
	_ = tw.Close()
	ha, _ := TarFingerprint(a)
	hb, _ := TarFingerprint(b.Bytes())
	if ha != hb || ha == "" {
		t.Fatal("fingerprint depends on order or bytecode")
	}
}

func TestUnderFolderLeadsWithTheOwnedFolder(t *testing.T) {
	out, err := underFolder(tarBody(map[string]string{"skills/a.json": "{}", "../x": "y"}), ".decurix", 1001)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	h, err := tr.Next()
	if err != nil || h.Name != ".decurix/" || h.Typeflag != tar.TypeDir || h.Uid != 1001 {
		t.Fatalf("first entry %+v %v", h, err)
	}
	names := []string{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, ".decurix/") || strings.Contains(n, "..") {
			t.Fatalf("entry escapes the folder: %v", names)
		}
	}
}
