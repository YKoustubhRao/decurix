package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// A native worker is a worker with no container: a private Win32 desktop of
// the person's own computer (internal/winhost) with the agent running on it.
// It is what the Windows app creates, on a machine that has no Docker and no
// Linux. Everything the Manager does to a desktop container has its
// counterpart here, so nothing above this package needs to know which kind it
// is talking to:
//
//	container            -> a helper process holding the private desktop, and
//	                        the agent it started there (everything the agent
//	                        starts inherits the desktop, and dies with the helper)
//	the /home volume     -> <Root>/<id>/home, with the same .decurix and work
//	                        folders the agent keeps in a container
//	container labels/env -> <Root>/<id>/worker.json
//	the agent's :8081    -> a loopback port of the worker's own, chosen at start
//	Xvnc + noVNC         -> the helper's pictures and input (winhost.Session)

// NativeRuntime is what a native worker is made of: where workers live, and
// the programs one runs.
type NativeRuntime struct {
	// Root holds one folder per worker.
	Root string
	// Python runs the agent. pythonw.exe: python.exe would open a console
	// window on the worker's screen.
	Python string
	// AgentDir holds native_worker.py and the decurix_agent package.
	AgentDir string
	// Soul is TheBot's engine (soul.exe).
	Soul string
	// UIHelper is decurix-ui.exe: how the agent sees and works its own
	// desktop (agent/windows/decurix-ui.cs).
	UIHelper string
	// ControlPlaneURL is where a worker calls back to: the primary listener
	// on loopback.
	ControlPlaneURL string
	// Helper runs winhost's helper script for one private desktop.
	Helper winhost.Starter
	// LocalModel makes the local model usable before a worker that reasons
	// with it starts: it may have to be downloaded first, and says what it is
	// waiting for. Nil when there is nothing to prepare.
	LocalModel func(ctx context.Context, say func(string)) error
}

// nativeMeta is worker.json: what a container's labels and environment record.
type nativeMeta struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Profile        string `json:"profile"`
	Harness        string `json:"harness"`
	Brain          string `json:"brain"`
	BrainModel     string `json:"brain_model,omitempty"`
	BrainVision    bool   `json:"brain_vision,omitempty"`
	Model          string `json:"model,omitempty"`
	CPUs           int64  `json:"cpus"`
	MemoryMB       int64  `json:"memory_mb"`
	CreatedAt      string `json:"created_at"`
	Specialization string `json:"specialization,omitempty"`
	ParentID       string `json:"parent_id,omitempty"`
	// Stopped is a worker a person stopped: it is not started again when the
	// control plane starts.
	Stopped bool `json:"stopped,omitempty"`
}

// nativeRun is one running worker.
type nativeRun struct {
	session   *winhost.Session
	agentPort int
	soulPort  int
	cdpPort   int
	startedAt time.Time
}

// nativeHost is the Manager's native backend.
type nativeHost struct {
	rt NativeRuntime

	mu      sync.Mutex
	running map[string]*nativeRun
	// crashed is why a worker that was running is not any more, until it is
	// started again.
	crashed map[string]string
	// starting holds workers a start is in flight for, so two requests do not
	// open two desktops for one worker.
	starting map[string]chan struct{}
}

// NewNativeManager creates a Manager whose desktops are native workers. It
// talks to no Docker daemon.
func NewNativeManager(rt NativeRuntime) *Manager {
	m := NewManager(nil)
	m.native = &nativeHost{rt: rt, running: map[string]*nativeRun{}, crashed: map[string]string{}, starting: map[string]chan struct{}{}}

	// What a worker is told to call back to (controlPlaneURL), and what the
	// hosted brain's proxy address is built from.
	if os.Getenv("DECURIX_CONTROL_PLANE_URL") == "" && rt.ControlPlaneURL != "" {
		os.Setenv("DECURIX_CONTROL_PLANE_URL", rt.ControlPlaneURL)
	}

	return m
}

// Native reports whether this Manager's desktops are native workers. A nil
// Manager (a test's server with no desktops at all) has none of either kind.
func (m *Manager) Native() bool { return m != nil && m.native != nil }

func (h *nativeHost) dir(id string) string      { return filepath.Join(h.rt.Root, id) }
func (h *nativeHost) home(id string) string     { return filepath.Join(h.rt.Root, id, "home") }
func (h *nativeHost) metaFile(id string) string { return filepath.Join(h.rt.Root, id, "worker.json") }

func (h *nativeHost) readMeta(id string) (nativeMeta, error) {
	var meta nativeMeta

	data, err := os.ReadFile(h.metaFile(id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return meta, fmt.Errorf("find desktop %s: %w", id, ErrNotFound)
		}
		return meta, err
	}

	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("read worker %s: %w", id, err)
	}

	return meta, nil
}

func (h *nativeHost) writeMeta(meta nativeMeta) error {
	if err := os.MkdirAll(h.dir(meta.ID), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}

	tmp := h.metaFile(meta.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, h.metaFile(meta.ID))
}

// run returns the worker's running session, forgetting one that has ended.
func (h *nativeHost) run(id string) *nativeRun {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.running[id]
	if r == nil {
		return nil
	}

	if !r.session.Alive() {
		delete(h.running, id)
		reason := "its desktop helper ended"
		if err := r.session.Err(); err != nil {
			reason = err.Error()
		}
		h.crashed[id] = reason

		return nil
	}

	return r
}

func (h *nativeHost) desktop(meta nativeMeta) Desktop {
	d := Desktop{
		ID:             meta.ID,
		Name:           meta.Name,
		Profile:        meta.Profile,
		Harness:        meta.Harness,
		Brain:          meta.Brain,
		Model:          meta.Model,
		ContainerID:    "native-" + meta.ID,
		Volume:         h.home(meta.ID),
		Status:         "stopped",
		URL:            desktopURL(meta.ID),
		CreatedAt:      meta.CreatedAt,
		CPUs:           meta.CPUs,
		MemoryMB:       meta.MemoryMB,
		Specialization: meta.Specialization,
		ParentID:       meta.ParentID,
	}

	if r := h.run(meta.ID); r != nil {
		d.Status = "running"
		d.StartedAt = r.startedAt.Format(time.RFC3339Nano)
	} else {
		h.mu.Lock()
		if reason := h.crashed[meta.ID]; reason != "" && !meta.Stopped {
			d.Crash = reason
		}
		h.mu.Unlock()
	}

	return d
}

func (h *nativeHost) list() ([]Desktop, error) {
	entries, err := os.ReadDir(h.rt.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var out []Desktop
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := h.readMeta(e.Name())
		if err != nil || meta.ID != e.Name() {
			continue
		}
		out = append(out, h.desktop(meta))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })

	return out, nil
}

// freePort asks the system for a loopback port nothing is listening on.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}

// notForNative are the variables buildDesktopEnv sets for a Linux desktop
// that mean nothing, or something wrong, on Windows.
var notForNative = map[string]bool{
	"VNC_PW": true, "VNC_RESOLUTION": true, "PATH": true, "GOROOT": true, "RUSTUP_HOME": true, "CARGO_HOME": true,
	"NPM_CONFIG_CACHE": true, "GOPATH": true, "GOMODCACHE": true, "PIP_CACHE_DIR": true, "HF_HOME": true,
}

// env is the environment a worker's agent starts with: what a desktop
// container is given, plus where this worker's own folders and ports are.
func (h *nativeHost) env(meta nativeMeta, agentPort, soulPort, cdpPort int) []string {
	brain := Brain{Kind: meta.Brain, Model: meta.BrainModel, Vision: meta.BrainVision}

	var env []string
	for _, entry := range buildDesktopEnv(meta.Name, meta.Harness, meta.Specialization, meta.ParentID, brain) {
		if name, _, _ := strings.Cut(entry, "="); !notForNative[name] {
			env = append(env, entry)
		}
	}

	home := h.home(meta.ID)
	decurix := filepath.Join(home, ".decurix")

	return append(env,
		"DECURIX_DIR="+decurix,
		"DECURIX_WORK_DIR="+filepath.Join(home, "work"),
		"DECURIX_LOG_DIR="+filepath.Join(h.dir(meta.ID), "logs"),
		"DECURIX_TASK_FILE="+filepath.Join(decurix, "current-task"),
		"DECURIX_AGENT_BIND=127.0.0.1",
		"DECURIX_AGENT_PORT="+strconv.Itoa(agentPort),
		"DECURIX_SOUL="+h.rt.Soul,
		"SOUL_HOST=127.0.0.1",
		"SOUL_PORT="+strconv.Itoa(soulPort),
		"THEBOT_URL=http://127.0.0.1:"+strconv.Itoa(soulPort),
		"WORKSPACE_ROOT="+filepath.Join(decurix, "ai"),
		"PYTHONPATH="+h.rt.AgentDir,
		"PYTHONUTF8=1",
		// The agent's two commands, gui and office, are this Python run on
		// the agent's modules (TheBot's internal/hostshell): python.exe, the
		// console build, beside the pythonw.exe the agent itself runs under.
		"DECURIX_PYTHON="+filepath.Join(filepath.Dir(h.rt.Python), "python.exe"),
		"DECURIX_UI_HELPER="+h.rt.UIHelper,
		// The worker's browser is started with a DevTools port of its own.
		"DECURIX_CDP_PORT="+strconv.Itoa(cdpPort),
	)
}

// start opens the worker's private desktop and starts its agent on it. A
// worker that is already running is left as it is.
func (h *nativeHost) start(ctx context.Context, id string, say func(string)) error {
	for {
		if h.run(id) != nil {
			return nil
		}

		h.mu.Lock()
		wait, busy := h.starting[id]
		if !busy {
			h.starting[id] = make(chan struct{})
		}
		h.mu.Unlock()

		if !busy {
			break
		}

		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	defer func() {
		h.mu.Lock()
		close(h.starting[id])
		delete(h.starting, id)
		h.mu.Unlock()
	}()

	meta, err := h.readMeta(id)
	if err != nil {
		return err
	}

	// Before the worker's environment is built: preparing the model is what
	// decides which model, and where, the worker is told to use.
	if h.rt.LocalModel != nil && meta.Brain == BrainOllama {
		if err := h.rt.LocalModel(ctx, say); err != nil {
			return fmt.Errorf("prepare the local model: %w", err)
		}
	}

	agentPort, err := freePort()
	if err != nil {
		return fmt.Errorf("start worker %s: %w", id, err)
	}
	soulPort, err := freePort()
	if err != nil {
		return fmt.Errorf("start worker %s: %w", id, err)
	}
	cdpPort, err := freePort()
	if err != nil {
		return fmt.Errorf("start worker %s: %w", id, err)
	}

	work := filepath.Join(h.home(id), "work")
	for _, dir := range []string{work, filepath.Join(h.home(id), ".decurix"), filepath.Join(h.dir(id), "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("start worker %s: %w", id, err)
		}
	}

	script := filepath.Join(h.rt.AgentDir, "native_worker.py")
	session, err := winhost.OpenDesktop(ctx, h.rt.Helper, "w_"+id, h.env(meta, agentPort, soulPort, cdpPort), h.rt.Python, `"`+script+`"`, work)
	if err != nil {
		return fmt.Errorf("start worker %s: %w", id, err)
	}

	h.mu.Lock()
	h.running[id] = &nativeRun{session: session, agentPort: agentPort, soulPort: soulPort, cdpPort: cdpPort, startedAt: time.Now()}
	delete(h.crashed, id)
	h.mu.Unlock()

	if meta.Stopped {
		meta.Stopped = false
		if err := h.writeMeta(meta); err != nil {
			log.Printf("worker %s: %v", id, err)
		}
	}

	return nil
}

// stop ends the worker's desktop and everything on it.
func (h *nativeHost) stop(id string, byPerson bool) error {
	meta, err := h.readMeta(id)
	if err != nil {
		return err
	}

	h.mu.Lock()
	r := h.running[id]
	delete(h.running, id)
	delete(h.crashed, id)
	h.mu.Unlock()

	if r != nil {
		r.session.Stop()
	}

	if byPerson && !meta.Stopped {
		meta.Stopped = true
		return h.writeMeta(meta)
	}

	return nil
}

func (h *nativeHost) delete(id string) error {
	if _, err := h.readMeta(id); err != nil {
		return err
	}

	if err := h.stop(id, false); err != nil {
		return err
	}

	// A program the agent started may hold a file for a moment after its
	// desktop is gone.
	var err error
	for i := 0; i < 20; i++ {
		if err = os.RemoveAll(h.dir(id)); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("delete worker %s: %w", id, err)
}

// create writes a new worker's record and starts it.
func (h *nativeHost) create(ctx context.Context, meta nativeMeta, say func(string)) error {
	if _, err := os.Stat(h.metaFile(meta.ID)); err == nil {
		return fmt.Errorf("desktop %s already exists", meta.Name)
	}

	if err := h.writeMeta(meta); err != nil {
		return fmt.Errorf("create worker: %w", err)
	}

	if err := h.start(ctx, meta.ID, say); err != nil {
		_ = os.RemoveAll(h.dir(meta.ID))
		return err
	}

	return nil
}

// agentURL is the worker's agent API, or why it cannot be reached.
func (h *nativeHost) agentURL(id string) (*url.URL, error) {
	if _, err := h.readMeta(id); err != nil {
		return nil, err
	}

	r := h.run(id)
	if r == nil {
		return nil, fmt.Errorf("desktop %s: %w", id, ErrNotRunning)
	}

	return &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(r.agentPort)}, nil
}

func (h *nativeHost) soulURL(id string) (*url.URL, error) {
	if _, err := h.readMeta(id); err != nil {
		return nil, err
	}

	r := h.run(id)
	if r == nil {
		return nil, fmt.Errorf("desktop %s: %w", id, ErrNotRunning)
	}

	return &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(r.soulPort)}, nil
}

func (h *nativeHost) sendRequest(ctx context.Context, cli *http.Client, id, content, taskID string) error {
	target, err := h.agentURL(id)
	if err != nil {
		return err
	}

	payload := map[string]string{"content": content}
	if taskID != "" {
		payload["task_id"] = taskID
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String()+"/request", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("send request to desktop %s: %w: %v", id, ErrAgentUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("desktop %s returned HTTP %d", id, resp.StatusCode)
	}

	return nil
}

// linuxHome is the home folder callers name a worker's files under, whatever
// kind of worker it is: CopyFiles, CopyOut and the agent's own messages all
// say /home/headless/...
const linuxHome = "/home/headless"

// homePath is a path under /home/headless as the folder it is for a native
// worker, or an error for a path outside the home.
func (h *nativeHost) homePath(id, p string) (string, error) {
	clean := path.Clean(p)
	if !strings.HasPrefix(clean+"/", linuxHome+"/") || clean == linuxHome {
		return "", fmt.Errorf("%q is not under %s", p, linuxHome)
	}

	return filepath.Join(h.home(id), filepath.FromSlash(strings.TrimPrefix(clean, linuxHome+"/"))), nil
}

func (h *nativeHost) copyFiles(id, dir string, files []File) error {
	if _, err := h.readMeta(id); err != nil {
		return err
	}

	target, err := h.homePath(id, dir)
	if err != nil {
		return fmt.Errorf("copy files: %w", err)
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("copy files into %s: %w", id, err)
	}

	for _, f := range files {
		name := path.Base(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || name == "/" || name == ".." || name == "" {
			return fmt.Errorf("copy files: %q is not a file name", f.Name)
		}

		if err := os.WriteFile(filepath.Join(target, name), f.Data, 0o644); err != nil {
			return fmt.Errorf("copy files into %s: %w", id, err)
		}
	}

	return nil
}

// copyOut tars a folder or file of the worker's home the way Docker's copy
// does: entries named from the folder's own name down.
func (h *nativeHost) copyOut(id, p string) (io.ReadCloser, error) {
	if _, err := h.readMeta(id); err != nil {
		return nil, err
	}

	source, err := h.homePath(id, p)
	if err != nil {
		return nil, fmt.Errorf("copy out: %w", err)
	}

	if _, err := os.Stat(source); err != nil {
		return nil, ErrNotFound
	}

	reader, writer := io.Pipe()

	go func() {
		tw := tar.NewWriter(writer)
		base := filepath.Dir(source)

		err := filepath.WalkDir(source, func(file string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			info, err := d.Info()
			if err != nil {
				return err
			}

			rel, err := filepath.Rel(base, file)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)

			if d.IsDir() {
				return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: info.ModTime()})
			}
			if !info.Mode().IsRegular() {
				return nil
			}

			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}

			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(data)), Mode: 0o644, ModTime: info.ModTime()}); err != nil {
				return err
			}

			_, err = tw.Write(data)

			return err
		})
		if err == nil {
			err = tw.Close()
		}

		writer.CloseWithError(err)
	}()

	return reader, nil
}

func (h *nativeHost) persistMemoryFile(id, filename, content string) error {
	if _, err := h.readMeta(id); err != nil {
		return err
	}

	name := filepath.Base(filepath.FromSlash(filename))
	if name == "." || name == string(filepath.Separator) || name == ".." {
		return fmt.Errorf("write memory: %q is not a file name", filename)
	}

	dir := filepath.Join(h.home(id), ".decurix", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write memory for %s: %w", id, err)
	}

	return os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimSpace(content)+"\n"), 0o644)
}

// session is the worker's desktop, for showing and driving its screen.
func (h *nativeHost) session(id string) (*winhost.Session, error) {
	if _, err := h.readMeta(id); err != nil {
		return nil, err
	}

	r := h.run(id)
	if r == nil {
		return nil, fmt.Errorf("desktop %s: %w", id, ErrNotRunning)
	}

	return r.session, nil
}

// screenshot is the worker's screen as a JPEG at most maxWidth wide.
func (h *nativeHost) screenshot(ctx context.Context, id string, maxWidth int) ([]byte, error) {
	session, err := h.session(id)
	if err != nil {
		return nil, err
	}

	shotCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	picture, err := session.Snapshot(shotCtx)
	if err != nil {
		return nil, err
	}

	if maxWidth <= 0 || picture.W <= maxWidth {
		return picture.JPEG, nil
	}

	img, err := jpeg.Decode(bytes.NewReader(picture.JPEG))
	if err != nil {
		return nil, fmt.Errorf("read the worker's screen: %w", err)
	}

	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)

	return encodeThumbnail(rgba, maxWidth)
}

// startAll starts every worker a person did not stop, when the control plane
// starts: a worker's desktop lives in the control plane's own process tree,
// so none survives a restart on its own.
func (h *nativeHost) startAll(ctx context.Context) {
	list, err := h.list()
	if err != nil {
		log.Printf("native workers: %v", err)
		return
	}

	for _, d := range list {
		meta, err := h.readMeta(d.ID)
		if err != nil || meta.Stopped {
			continue
		}

		if err := h.start(ctx, d.ID, nil); err != nil {
			log.Printf("native workers: %v", err)
		}
	}
}

// stopAll ends every worker's desktop, for shutdown: nothing on a private
// desktop may outlive the control plane that shows it.
func (h *nativeHost) stopAll() {
	h.mu.Lock()
	runs := h.running
	h.running = map[string]*nativeRun{}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(s *winhost.Session) {
			defer wg.Done()
			s.Stop()
		}(r.session)
	}
	wg.Wait()
}

// StartNativeWorkers starts the workers that were running when the control
// plane last stopped. A no-op for a Manager of containers.
func (m *Manager) StartNativeWorkers(ctx context.Context) {
	if m.native != nil {
		m.native.startAll(ctx)
		m.invalidateList()
	}
}

// StopNativeWorkers ends every native worker's desktop. A no-op for a Manager
// of containers.
func (m *Manager) StopNativeWorkers() {
	if m.native != nil {
		m.native.stopAll()
	}
}

// NativeSession is a native worker's desktop: its pictures, and the mouse and
// keyboard that drive it. ErrNotRunning for a worker that is stopped.
func (m *Manager) NativeSession(rawID string) (*winhost.Session, error) {
	if m.native == nil {
		return nil, errors.New("this control plane's desktops are containers")
	}

	id := sanitizeName(rawID)
	m.touchViewed(id)

	return m.native.session(id)
}

// nativeProvision is BeginProvisioning for a native worker: the same record a
// dashboard polls, with nothing slower behind it than starting two programs.
func (m *Manager) nativeProvision(
	name, profile, harness string,
	brain Brain,
	cpus, memoryMB int64,
	specialization, parentID string,
	onStarted func(d *Desktop) error,
) (*ProvisioningDesktop, error) {
	id := sanitizeName(name)
	if id == "" {
		return nil, fmt.Errorf("invalid desktop name")
	}

	m.provisioningMu.Lock()
	if _, exists := m.provisioning[id]; exists {
		m.provisioningMu.Unlock()
		return nil, fmt.Errorf("desktop %s is already being created", name)
	}
	if _, err := os.Stat(m.native.metaFile(id)); err == nil {
		m.provisioningMu.Unlock()
		return nil, fmt.Errorf("desktop %s already exists", name)
	}

	meta := nativeMeta{
		ID: id, Name: name, Profile: profile, Harness: harness,
		Brain: brain.kind(), BrainModel: brain.Model, BrainVision: brain.Vision,
		CPUs: cpus, MemoryMB: memoryMB, CreatedAt: time.Now().Format(time.RFC3339),
		Specialization: specialization, ParentID: parentID,
	}
	meta.Model = desktopModel(buildDesktopEnv(name, harness, specialization, parentID, brain))

	prov := &ProvisioningDesktop{
		ID: id, Name: name, Profile: profile, Harness: harness, Brain: meta.Brain, Model: meta.Model,
		CPUs: cpus, MemoryMB: memoryMB, Status: "provisioning", Stage: "starting",
		Message: "Starting the worker...", Progress: 40, URL: desktopURL(id), CreatedAt: meta.CreatedAt,
		Specialization: specialization, ParentID: parentID,
	}
	m.provisioning[id] = prov
	snapshot := *prov
	m.provisioningMu.Unlock()

	go func() {
		defer m.invalidateList()

		// Long: the first worker on the local model waits for that model to be
		// downloaded.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()

		say := func(message string) {
			m.provisioningMu.Lock()
			prov.Message = message
			m.provisioningMu.Unlock()
		}

		if err := m.native.create(ctx, meta, say); err != nil {
			log.Printf("desktop provisioning failed for %s: %v", id, err)
			m.provisioningMu.Lock()
			prov.Status, prov.Error, prov.Message = "failed", err.Error(), fmt.Sprintf("Creation failed: %v", err)
			m.provisioningMu.Unlock()

			time.AfterFunc(60*time.Second, func() {
				m.provisioningMu.Lock()
				delete(m.provisioning, id)
				m.provisioningMu.Unlock()
			})

			return
		}

		if onStarted != nil {
			created := m.native.desktop(meta)
			if err := onStarted(&created); err != nil {
				log.Printf("desktop onStarted callback for %s: %v", id, err)
			}
		}

		m.provisioningMu.Lock()
		prov.Stage, prov.Message, prov.Progress, prov.Status, prov.Ready = "ready", "Desktop is ready!", 100, "running", true
		m.provisioningMu.Unlock()
		m.invalidateList()

		time.Sleep(5 * time.Second)
		m.provisioningMu.Lock()
		delete(m.provisioning, id)
		m.provisioningMu.Unlock()
	}()

	return &snapshot, nil
}

// errNativeNoClaudeLogin is the shared Claude Code login asked of a control
// plane with no container to run the claude CLI in.
var errNativeNoClaudeLogin = errors.New("the shared Claude Code login needs a desktop container, which this control plane does not have")
