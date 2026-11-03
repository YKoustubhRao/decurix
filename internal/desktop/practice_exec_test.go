package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// execDaemon is a stand-in Docker daemon for ExecPracticeAsWorker and DesktopImageID: containers by id with their
// labels, one exec whose output it writes, and the desktop images.
type execDaemon struct {
	t      *testing.T
	mu     sync.Mutex
	labels map[string]map[string]string
	output []byte
	execs  []string // the users execs ran as
	cmds   [][]string
}

func (d *execDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := r.URL.Path
	if i := strings.Index(p[1:], "/"); strings.HasPrefix(p, "/v1.") && i > 0 {
		p = p[i+1:]
	}
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(p, "/")
	switch {
	case strings.HasPrefix(p, "/images/decurix-desktop:basic/json"):
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:" + strings.Repeat("c", 64), "Os": "linux"})
	case strings.HasPrefix(p, "/images/"):
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "no such image"})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		labels, ok := d.labels[parts[2]]
		if !ok {
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "no such container"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": parts[2], "Config": map[string]any{"Labels": labels}})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/exec"):
		var body struct {
			User string
			Cmd  []string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.execs = append(d.execs, body.User)
		d.cmds = append(d.cmds, body.Cmd)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": "ex1"})
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/start"):
		_, _ = io.Copy(io.Discard, r.Body)
		conn, buff, err := w.(http.Hijacker).Hijack()
		if err != nil {
			d.t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buff.WriteString("HTTP/1.1 101 UPGRADED" + crlf + "Content-Type: application/vnd.docker.raw-stream" + crlf + "Connection: Upgrade" + crlf + "Upgrade: tcp" + crlf + crlf)
		_ = buff.Flush()
		_, _ = stdcopy.NewStdWriter(conn, stdcopy.Stdout).Write(d.output)
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
		_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": 3})
	default:
		d.t.Errorf("unexpected Docker request: %s %s", r.Method, p)
		w.WriteHeader(404)
	}
}

func execManager(t *testing.T, d *execDaemon) *Manager {
	t.Helper()
	ts := httptest.NewServer(d)
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")), client.WithVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return NewManager(cli)
}

// Only a practice desktop runs a command this way: a worker's own desktop and a mock app are refused before any
// exec is made.
func TestExecPracticeAsWorkerRefusesAnythingButAPracticeDesktop(t *testing.T) {
	d := &execDaemon{t: t, output: []byte("ok\n"), labels: map[string]map[string]string{
		"practice": {PracticeLabel: "true"},
		"worker":   {"decurix-desktop": "true"},
		"mock":     {PracticeLabel: "true", "decurix-practice-mock": "true"},
	}}
	m := execManager(t, d)
	for _, id := range []string{"worker", "mock"} {
		if _, _, err := m.ExecPracticeAsWorker(context.Background(), id, []string{"true"}, time.Second); !errors.Is(err, ErrNotPractice) {
			t.Errorf("%s: err = %v, want ErrNotPractice", id, err)
		}
	}
	if _, _, err := m.ExecPracticeAsWorker(context.Background(), "gone", []string{"true"}, time.Second); err == nil {
		t.Error("a container that does not exist ran")
	}
	if len(d.execs) != 0 {
		t.Fatalf("an exec was made in a refused container: %v", d.execs)
	}

	exit, out, err := m.ExecPracticeAsWorker(context.Background(), "practice", []string{"gui", "type", "x"}, 5*time.Second)
	if err != nil || exit != 3 || string(out) != "ok\n" {
		t.Fatalf("practice exec = %d %q %v", exit, out, err)
	}
	if len(d.execs) != 1 || d.execs[0] != "headless" {
		t.Fatalf("ran as %v", d.execs)
	}
	cmd := strings.Join(d.cmds[0], " ")
	if !strings.HasPrefix(cmd, "/usr/bin/env -i ") || !strings.HasSuffix(cmd, " gui type x") {
		t.Fatalf("command = %q, want the worker's empty environment", cmd)
	}
}

func TestExecPracticeAsWorkerCapsItsOutput(t *testing.T) {
	big := bytes.Repeat([]byte("y"), practiceExecOutputLimit+5000)
	d := &execDaemon{t: t, output: big, labels: map[string]map[string]string{"practice": {PracticeLabel: "true"}}}
	m := execManager(t, d)
	_, out, err := m.ExecPracticeAsWorker(context.Background(), "practice", []string{"yes"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, big[:practiceExecOutputLimit]) || !bytes.Contains(out, []byte("[5000 more bytes of output not kept]")) {
		t.Fatalf("output %d bytes, tail %q", len(out), out[max(0, len(out)-60):])
	}
	if len(out) > practiceExecOutputLimit+100 {
		t.Fatalf("output %d bytes, over the cap", len(out))
	}
}

func TestDesktopImageIDIsTheImmutableID(t *testing.T) {
	m := execManager(t, &execDaemon{t: t})
	id, err := m.DesktopImageID(context.Background(), "")
	if err != nil || id != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("DesktopImageID = %q, %v", id, err)
	}
	if _, err := m.DesktopImageID(context.Background(), "nonsense"); err == nil {
		t.Fatal("an unknown profile resolved")
	}
	if _, err := m.DesktopImageID(context.Background(), "ai"); err == nil {
		t.Fatal("a missing image resolved")
	}
}
