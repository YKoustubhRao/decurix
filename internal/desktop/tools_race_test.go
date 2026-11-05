package desktop

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// slowTools is a daemon whose tools populate takes as long as release stays open, and whose one desktop "box"
// takes startDelay to reach running after ContainerStart (as Docker does).
type slowTools struct {
	*toolsDaemon
	release    chan struct{}
	startDelay time.Duration

	mu        sync.Mutex
	state     container.ContainerState
	toldEnv   []string
	populated chan struct{}
	once      sync.Once
}

func (d *slowTools) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	status := make(chan container.WaitResponse, 1)
	go func() {
		if d.release != nil {
			<-d.release
		}
		status <- container.WaitResponse{StatusCode: 0}
	}()
	return status, make(chan error)
}

func (d *slowTools) ContainerList(_ context.Context, o container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return []container.Summary{{Names: []string{"/desktop_box"}, State: d.state}}, nil
}

func (d *slowTools) ContainerStop(_ context.Context, name string, _ container.StopOptions) error {
	d.mu.Lock()
	d.state = container.StateExited
	d.mu.Unlock()
	return nil
}

func (d *slowTools) ContainerStart(_ context.Context, name string, _ container.StartOptions) error {
	if name == "desktop_box" {
		time.Sleep(d.startDelay)
		d.mu.Lock()
		d.state = container.StateRunning
		d.mu.Unlock()
	}
	return nil
}

func (d *slowTools) ContainerCreate(_ context.Context, cfg *container.Config, _ *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	if strings.HasPrefix(name, "decurix_tools_init_") {
		d.mu.Lock()
		d.toldEnv = append([]string{}, cfg.Env...)
		d.mu.Unlock()
		d.once.Do(func() { close(d.populated) })
	}
	return container.CreateResponse{ID: "id-" + name}, nil
}

// /api/docker/status (ToolsRefreshWaiting) answers while a populate copies the volume (review CP-B B3): it never
// takes the lock the populate holds.
func TestStatusAnswersDuringAPopulate(t *testing.T) {
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	d := &slowTools{toolsDaemon: &toolsDaemon{fakeAPI: fake}, release: make(chan struct{}), populated: make(chan struct{}),
		state: container.StateExited}
	m := newManagerWith(d)
	go func() { _ = m.EnsureToolsVolume(context.Background()) }()
	<-d.populated
	answered := make(chan bool, 1)
	go func() { answered <- m.ToolsRefreshWaiting(context.Background()) }()
	select {
	case <-answered:
	case <-time.After(500 * time.Millisecond):
		close(d.release)
		t.Fatal("ToolsRefreshWaiting blocked for the whole populate: /api/docker/status hangs while the tools volume is copied")
	}
	close(d.release)
}

// A restart while a tools refresh waits does not apply it: the desktop is back at once, with no copy under it
// (review CP-B B2). A stop does apply it.
func TestOnlyAStopAppliesAWaitingRefresh(t *testing.T) {
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	d := &slowTools{toolsDaemon: &toolsDaemon{fakeAPI: fake, imageID: "sha256:tools-2"}, populated: make(chan struct{}),
		startDelay: 100 * time.Millisecond, state: container.StateRunning}
	m := newManagerWith(d)
	old := "sha256:tools-1"
	m.toolsApplied.Store(&old)
	m.toolsWaiting.Store(true)

	if err := m.Restart(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.populated:
		t.Fatal("a restart started a tools refresh: it would copy over /opt/tools as the desktop boots")
	case <-time.After(300 * time.Millisecond):
	}

	if err := m.Stop(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.populated:
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the last desktop did not apply the waiting refresh")
	}
	d.mu.Lock()
	env := d.toldEnv
	d.mu.Unlock()
	if len(env) != 1 || env[0] != "DECURIX_DESKTOPS_RUNNING=0" {
		t.Fatalf("the populate after the stop was told %v", env)
	}
}

// A desktop started while a populate replaces the tools volume waits until the copy is done (review CP-B B2), by
// every way a desktop starts: Start, and Restart's start after its stop. (Create starts through the same
// startDesktopContainer; TestEveryDesktopStartTakesTheToolsLock holds every start to it.)
func TestAStartWaitsForAPopulate(t *testing.T) {
	for name, start := range map[string]func(m *Manager) error{
		"Start":   func(m *Manager) error { return m.Start(context.Background(), "box") },
		"Restart": func(m *Manager) error { return m.Restart(context.Background(), "box") },
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAPI{}
			fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
			d := &slowTools{toolsDaemon: &toolsDaemon{fakeAPI: fake}, release: make(chan struct{}), populated: make(chan struct{}),
				state: container.StateExited}
			m := newManagerWith(d)
			go func() { _ = m.EnsureToolsVolume(context.Background()) }()
			<-d.populated
			started := make(chan struct{})
			go func() {
				_ = start(m)
				close(started)
			}()
			select {
			case <-started:
				t.Fatal("a desktop started while the tools volume was being replaced")
			case <-time.After(300 * time.Millisecond):
			}
			d.mu.Lock()
			running := d.state == container.StateRunning
			d.mu.Unlock()
			if running {
				t.Fatal("the desktop reached running during the copy")
			}
			close(d.release)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("the start never went ahead after the populate")
			}
		})
	}
}

// Every start of a container in this package goes through startDesktopContainer, which holds the tools lock: a
// desktop's create (runProvisioning), Start or Restart, a practice desktop's create and reset, a practice mock, and
// the Claude login container (which mounts /opt/tools). A start that called the client directly would boot under a
// copy. The starts named here are of containers that never mount the shared tools volume.
func TestEveryDesktopStartTakesTheToolsLock(t *testing.T) {
	allowed := map[string]string{
		"startDesktopContainer": "the one desktop start, under toolsRW",
		"PopulateToolsVolume":   "the tools init container itself",
		"homeHelper":            "the home-copy helper, which mounts no tools",
		"RunQualification":      "a readiness container, with no tools volume",
	}
	files, _ := filepath.Glob("*.go")
	found, locked := 0, 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "startDesktopContainer" {
					locked++
				}
				if !ok || sel.Sel.Name != "ContainerStart" {
					return true
				}
				found++
				if allowed[fn.Name.Name] == "" {
					t.Errorf("%s: %s starts a container outside startDesktopContainer (the tools lock)", file, fn.Name.Name)
				}
				return true
			})
		}
	}
	// Create, Start, Restart, a practice desktop's create and reset, a practice mock, the Claude login.
	if found < 4 || locked < 7 {
		t.Fatalf("%d direct and %d locked container starts found: the walk is not reading the package", found, locked)
	}
}
