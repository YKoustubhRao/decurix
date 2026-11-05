package httpapi

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// workerTarDaemon is the exec half of a stand-in Docker daemon: a desktop's files now go in and out as the worker,
// by tar run through exec (internal/desktop/homefs.go), not by Docker's archive API. It extracts what an exec of
// `tar -x` is fed into onFile and answers any other exec with exit 0, recording each exec's user and command.
type workerTarDaemon struct {
	mu     sync.Mutex
	seq    int
	execs  map[string]container.ExecOptions
	onFile func(name, data string)
}

// serve answers the exec endpoints and reports whether r was one of them.
func (d *workerTarDaemon) serve(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/exec") && strings.Contains(p, "/containers/"):
		var opts container.ExecOptions
		_ = json.NewDecoder(r.Body).Decode(&opts)
		d.mu.Lock()
		if d.execs == nil {
			d.execs = map[string]container.ExecOptions{}
		}
		d.seq++
		id := fmt.Sprintf("exec%d", d.seq)
		d.execs[id] = opts
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(container.ExecCreateResponse{ID: id})
		return true
	case r.Method == http.MethodPost && strings.Contains(p, "/exec/") && strings.HasSuffix(p, "/start"):
		id := strings.TrimSuffix(p[strings.Index(p, "/exec/")+len("/exec/"):], "/start")
		d.mu.Lock()
		opts := d.execs[id]
		d.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body) // the start request's own JSON, before the stream
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return true
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n" +
			"Connection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = buf.Flush()
		if opts.AttachStdin && strings.Contains(strings.Join(opts.Cmd, " "), "tar -x") {
			d.extract(buf.Reader)
		}
		if opts.AttachStdin {
			_, _ = io.Copy(io.Discard, buf.Reader) // to the caller's end of stdin, so closing is no reset
		}
		_, _ = stdcopy.NewStdWriter(conn, stdcopy.Stdout).Write(nil)
		return true
	case r.Method == http.MethodGet && strings.Contains(p, "/exec/") && strings.HasSuffix(p, "/json"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(container.ExecInspect{ExitCode: 0})
		return true
	}
	return false
}

func (d *workerTarDaemon) extract(r *bufio.Reader) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return
		}
		data, _ := io.ReadAll(tr)
		if hdr.Typeflag == tar.TypeReg && d.onFile != nil {
			d.onFile(hdr.Name, string(data))
		}
	}
}

// users lists the user every exec ran as.
func (d *workerTarDaemon) users() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, o := range d.execs {
		out = append(out, o.User)
	}
	return out
}
