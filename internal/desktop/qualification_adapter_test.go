package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// TestQualificationDockerAdapter exercises the full Docker API sequence without
// a real daemon; live isolation remains covered by the explicitly enabled test.
func TestQualificationDockerAdapter(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	var mu sync.Mutex
	copiedIn, readOut, removed := false, false, false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": image, "Os": "linux", "Config": map[string]any{"Env": []string{"API_KEY=hidden"}}})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			host := payload["HostConfig"].(map[string]any)
			if host["NetworkMode"] != "none" || host["ReadonlyRootfs"] != true {
				t.Error("isolation lost in serialized Docker request")
			}
			for _, e := range payload["Env"].([]any) {
				if strings.Contains(e.(string), "hidden") {
					t.Error("inherited image key leaked")
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "practice-container"})
		case strings.HasSuffix(r.URL.Path, "/containers/practice-container/start"):
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/containers/practice-container/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "practice-container", "State": map[string]any{"Running": true, "Paused": false}})
		case strings.HasSuffix(r.URL.Path, "/containers/practice-container/exec"):
			var payload struct {
				Cmd         []string
				WorkingDir  string
				User        string
				AttachStdin bool
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload.Cmd[0] != "/usr/bin/env" || payload.Cmd[1] != "-i" || payload.User != "1001:1001" {
				t.Error("unsafe exec context")
			}
			joined := strings.Join(payload.Cmd, " ")
			id := "run-exec"
			switch {
			case strings.Contains(joined, "tar -x -C /work"):
				id = "in-exec"
			case strings.Contains(joined, "kill -s STOP"):
				id = "out-exec"
			case payload.WorkingDir != "/work":
				t.Error("program not run in /work")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path[strings.Index(r.URL.Path, "/exec/"):], "/exec/"), "/start")
			// Consume the HTTP request before upgrading; otherwise Windows
			// resets a connection closed with unread request-body bytes.
			_, _ = io.Copy(io.Discard, r.Body)
			conn, buff, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = buff.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buff.Flush()
			switch id {
			case "in-exec":
				tr := tar.NewReader(buff)
				copied := map[string]string{}
				for {
					h, err := tr.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Error(err)
						break
					}
					b, _ := io.ReadAll(tr)
					copied[h.Name] = string(b)
				}
				_, _ = io.Copy(io.Discard, buff)
				if len(copied) != 2 || copied["input.txt"] != "input" || copied["main.py"] != "print('work')" {
					t.Error("wrong executable or fixture copy")
				}
				for _, s := range copied {
					if strings.Contains(s, "SECRET_EXPECTATION") {
						t.Error("holdout answer leaked to container")
					}
				}
				mu.Lock()
				copiedIn = true
				mu.Unlock()
			case "out-exec":
				var b bytes.Buffer
				tw := tar.NewWriter(&b)
				for name, text := range map[string]string{"main.py": "print('work')", "input.txt": "input", "out.txt": "SECRET_EXPECTATION"} {
					_ = tw.WriteHeader(&tar.Header{Name: "work/" + name, Typeflag: tar.TypeReg, Size: int64(len(text))})
					_, _ = tw.Write([]byte(text))
				}
				_ = tw.Close()
				_, _ = stdcopy.NewStdWriter(conn, stdcopy.Stdout).Write(b.Bytes())
				mu.Lock()
				readOut = true
				mu.Unlock()
			default:
				mu.Lock()
				ok := copiedIn
				mu.Unlock()
				if !ok {
					t.Error("program ran before its files were copied in")
				}
				_, _ = stdcopy.NewStdWriter(conn, stdcopy.Stdout).Write([]byte("worked"))
			}
		case strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": 0})
		case strings.HasSuffix(r.URL.Path, "/containers/practice-container") && r.Method == "DELETE":
			mu.Lock()
			removed = true
			mu.Unlock()
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")), client.WithVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	d := qualification.Definition{Image: image, Runtime: qualification.RuntimeVersion, Language: "python", Script: "print('work')", TimeoutSeconds: 5}
	c := qualification.Case{ID: "ordinary", Expected: "complete", Inputs: map[string][]byte{"input.txt": []byte("input")}, Checks: []qualification.Check{{File: "out.txt", Kind: "text_equals", Text: "SECRET_EXPECTATION"}}}
	result, err := NewManager(cli).RunQualification(context.Background(), d, c)
	if !qualification.Verify(c, result, err).Passed {
		t.Fatalf("adapter failed: %v %+v", err, result)
	}
	mu.Lock()
	defer mu.Unlock()
	if !removed {
		t.Fatal("practice container left behind")
	}
	if !readOut {
		t.Fatal("artifacts were not read through the freezing exec")
	}
}
