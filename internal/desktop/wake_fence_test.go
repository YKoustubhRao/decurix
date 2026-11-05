package desktop

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// pausedBox is a daemon whose desktop "box" is paused on its own network.
func pausedBox() *fakeAPI {
	fake := &fakeAPI{}
	fake.inspect = func(string) (container.InspectResponse, error) {
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true, Paused: true}},
			Config:            &container.Config{Labels: map[string]string{GateLabel: "1"}},
			NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
				DesktopNetworkName("box"): {IPAddress: "10.213.1.2"},
			}},
		}, nil
	}
	return fake
}

// On the Windows app's WSL host, a paused desktop is not woken while the fence between desktops does not hold - by
// the proxy (a screen or a page opened), by a post to its gate, by Unpause or by Start - and is once it holds
// (review NET-R3-2: auto-wake went around Start's check).
func TestAutoWakeIsRefusedWithoutTheFence(t *testing.T) {
	onWSLHost(t, "state=failed\npool=10.213.0.0/16\n")
	ctx := context.Background()
	fake := pausedBox()
	m := newManagerWith(fake)
	if _, _, err := m.ProxyTarget(ctx, "box"); !errors.Is(err, ErrNoFence) {
		t.Errorf("the proxy woke the desktop: %v", err)
	}
	if _, _, err := m.ScreenStreamAddress(ctx, "box"); !errors.Is(err, ErrNoFence) {
		t.Errorf("the screen stream woke the desktop: %v", err)
	}
	if err := m.wake(ctx, "desktop_box"); !errors.Is(err, ErrNoFence) {
		t.Errorf("the gate's wake woke the desktop: %v", err)
	}
	if err := m.Unpause(ctx, "box"); !errors.Is(err, ErrNoFence) {
		t.Errorf("Unpause woke the desktop: %v", err)
	}
	if err := m.Start(ctx, "box"); !errors.Is(err, ErrNoFence) {
		t.Errorf("Start woke the desktop: %v", err)
	}
	if n := len(fake.called("ContainerUnpause")); n != 0 {
		t.Fatalf("%d unpauses without the fence", n)
	}

	onWSLHost(t, "state=ok\npool=10.213.0.0/16\n")
	if err := newManagerWith(pausedBox()).Unpause(ctx, "box"); err != nil {
		t.Fatalf("with the fence holding: %v", err)
	}
}

// Every way a desktop is made to run goes through the fence's one check: in this package, ContainerStart and
// ContainerUnpause are called only by startDesktopContainer and wakeDesktop, apart from what is not a desktop made to
// run (a stop or a delete unpauses only to end it; a home-copy helper; the tools container; practice and readiness
// containers, on their own network).
func TestEveryStartAndWakeGoesThroughTheFenceCheck(t *testing.T) {
	allowed := map[string]bool{
		"startDesktopContainer": true, "wakeDesktop": true, // the two ways
		"stopContainer": true, "Delete": true, // unpaused only to be stopped or removed
		"homeHelper": true, "refreshTools": true, "runTools": true, "UnpausePractice": true, // not desktops
	}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
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
				if !ok || (sel.Sel.Name != "ContainerStart" && sel.Sel.Name != "ContainerUnpause") {
					return true
				}
				seen++
				if !allowed[fn.Name.Name] && path != "qualification.go" && path != "tools.go" {
					t.Errorf("%s: %s calls %s; a desktop runs only through startDesktopContainer or wakeDesktop",
						fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
	if seen < 4 {
		t.Fatalf("found %d calls: the walk did not see the package", seen)
	}
}
