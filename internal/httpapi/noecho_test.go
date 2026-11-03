package httpapi

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/dag"
)

// No handler sends an error's own words to a client (the CLAUDE.md rule behind jsonError): a store's, Docker's or the
// network's text can carry paths, addresses and internals, and the gated listener reaches the same handlers. This walks
// every call that writes a response - http.Error, jsonStatusError, jsonResponse, a ResponseWriter's Write - and fails
// on any X.Error() in what it is given. A reason meant for the person goes through refusal, providerSaid or
// viewerReason (json.go), whose names say what they may be given; an internal one through internalError or jsonError.
func TestNoResponseEchoesAnError(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	writers := map[string]bool{"http.Error": true, "jsonStatusError": true, "jsonResponse": true, "w.Write": true}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !writers[exprName(call.Fun)] {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(m ast.Node) bool {
					inner, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" && len(inner.Args) == 0 {
						t.Errorf("%s: %s sends %s.Error() to the client", fset.Position(inner.Pos()), exprName(call.Fun), exprName(sel.X))
					}
					return true
				})
			}
			return true
		})
	}
}

// exprName is a call's function or a selector's receiver as written: "http.Error", "w.Write", "err".
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	}
	return ""
}

// A DAG edit the store refused shows its reason; any other DAG error is logged and answered as internal.
func TestADagErrorShowsOnlyARefusalsReason(t *testing.T) {
	rec := httptest.NewRecorder()
	dagReply(rec, nil, &dag.InputError{Msg: "that edge would create a cycle"}, "no such DAG")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cycle") {
		t.Fatalf("refusal: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	dagReply(rec, nil, errors.New("dag: add node: sql: database is locked at /data/decurix.db"), "no such DAG")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "decurix.db") {
		t.Fatalf("internal: %d %s", rec.Code, rec.Body)
	}
}
