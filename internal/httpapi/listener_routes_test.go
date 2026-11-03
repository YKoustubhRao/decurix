package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/handoff"
	"github.com/YKoustubhRao/decurix/internal/knowledge"
	"github.com/YKoustubhRao/decurix/internal/skillhub"
	"github.com/YKoustubhRao/decurix/internal/workqueue"
)

// The listener tests below walk the routes as cmd/decurix/main.go registers them, on a Server with every optional
// store set, so that no route is left out because its store was nil, and nothing is skipped: the review of
// 2026-11-02 found the only such test skipping when registration panicked, leaving the handoff, knowledge and
// skillhub callbacks out, and the gated listener never walked at all.

// fullServer is a Server with every store that adds routes set.
func fullServer(t *testing.T) *Server {
	t.Helper()
	db := testDB(t)
	s := &Server{}
	var err error
	if s.SkillHub, err = skillhub.NewStore(db); err != nil {
		t.Fatal(err)
	}
	if s.Knowledge, err = knowledge.NewStore(db); err != nil {
		t.Fatal(err)
	}
	if s.Queue, err = workqueue.NewStore(db); err != nil {
		t.Fatal(err)
	}
	s.Handoffs = &handoff.Service{}
	store, err := access.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Access = &access.Handler{Store: store}
	return s
}

// listenerMuxes builds the two muxes the way main.go does: the primary one with everything, the gated one with
// RegisterRoutes and the sign-in routes only.
func listenerMuxes(s *Server) (primary, gated *http.ServeMux) {
	primary = http.NewServeMux()
	s.RegisterRoutes(primary)
	s.RegisterLocalRoutes(primary)
	s.RegisterSimulationRoutes(primary, nil)
	s.RegisterAccessRoutes(primary)
	s.RegisterAccessLocalRoutes(primary)

	gated = http.NewServeMux()
	s.RegisterRoutes(gated)
	s.RegisterAccessRoutes(gated)
	return primary, gated
}

// literalPaths is every string literal starting with "/" in the body of func name in file (a path of this package,
// or of another one under internal/): the routes a Register function names, whether as HandleFunc's argument or as
// a key of a map it loops over.
func literalPaths(t *testing.T, file, name string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	found := false
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(v, "/") {
				paths = append(paths, v)
			}
			return true
		})
	}
	if !found {
		t.Fatalf("no func %s in %s", name, file)
	}
	return paths
}

// localOnlyPaths is every route registered on the primary listener alone: what RegisterLocalRoutes,
// RegisterSimulationRoutes and the access admin routes name, the work queue's and the handoffs'.
func localOnlyPaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	paths = append(paths, literalPaths(t, "server.go", "RegisterLocalRoutes")...)
	paths = append(paths, literalPaths(t, "simulation_routes.go", "RegisterSimulationRoutes")...)
	paths = append(paths, literalPaths(t, filepath.Join("..", "access", "http.go"), "RegisterAdmin")...)
	paths = append(paths, literalPaths(t, filepath.Join("..", "handoff", "handler.go"), "Register")...)
	paths = append(paths, workqueue.Paths()...)
	sort.Strings(paths)
	return paths
}

// Nothing registered for the primary listener alone is served on the gated one (the one a tunnel and the LAN
// reach): not the host bridge, not the access code, not the backup, not the keys - found by walking what the
// Register functions name, not from a list kept by hand beside them.
func TestNoPrimaryOnlyRouteIsOnTheGatedListener(t *testing.T) {
	s := fullServer(t)
	primary, gated := listenerMuxes(s)

	paths := localOnlyPaths(t)
	if len(paths) < 40 {
		t.Fatalf("only %d primary-only routes found: the walk is not reading the Register functions (%v)", len(paths), paths)
	}
	for _, path := range paths {
		probe := path
		if strings.HasSuffix(probe, "/") {
			probe += "x" // a prefix route
		}
		req := httptest.NewRequest(http.MethodGet, probe, nil)
		if _, pattern := primary.Handler(req); pattern != path {
			t.Errorf("%s: the primary listener serves it as %q, not as itself - the walk found a route that is not one", path, pattern)
		}
		if _, pattern := gated.Handler(req); pattern == path {
			t.Errorf("%s is registered on the gated listener: whoever holds the access code reaches it", path)
		}
	}
}

// Every route a desktop may call back on is one the primary listener serves - a typo or a route left out would fail
// an agent's callback closed, silently - and none is a page or API the gated listener has that a desktop should not.
func TestEveryDesktopCallbackIsAPrimaryRoute(t *testing.T) {
	primary, _ := listenerMuxes(fullServer(t))
	for _, route := range containerRoutes {
		probe := route.path
		if route.prefix {
			probe += "x"
		}
		_, pattern := primary.Handler(httptest.NewRequest(http.MethodGet, probe, nil))
		if pattern != route.path {
			t.Errorf("desktop callback %s is served as %q on the primary listener, not as itself", route.path, pattern)
		}
	}
}

// From a desktop's address, the guard of the primary listener lets through only its callbacks: every other route the
// listeners serve, walked from the Register functions, is refused before any handler runs.
func TestADesktopIsRefusedEveryRouteButItsCallbacks(t *testing.T) {
	_, h := guardedServer(t)
	paths := append(localOnlyPaths(t), literalPaths(t, "server.go", "RegisterRoutes")...)
	paths = append(paths, literalPaths(t, filepath.Join("..", "access", "http.go"), "RegisterAuth")...)
	callback := map[string]bool{}
	for _, route := range containerRoutes {
		if !route.prefix {
			callback[route.path] = true
		}
	}
	checked := 0
	for _, path := range paths {
		probe := path
		if strings.HasSuffix(probe, "/") {
			probe += "x"
		}
		if callback[path] || containerMayCall(http.MethodGet, probe) || containerMayCall(http.MethodPost, probe) {
			continue
		}
		checked++
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			w := guardDo(h, method, probe, "172.30.0.5:40000", map[string]string{"Host": "decurix:8080"})
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s from a desktop: got %d, want 403", method, probe, w.Code)
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d routes checked: the walk is not reading the Register functions", checked)
	}
}
