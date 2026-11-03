package httpapi

import (
	"errors"
	"fmt"
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
// every call that writes a response and fails on an error's text in what it is given (echoesIn). A reason meant for
// the person goes through refusal, providerSaid or viewerReason (json.go), whose names say what they may be given; an
// internal one through internalError or jsonError.
func TestNoResponseEchoesAnError(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, found := range echoesIn(fset, f) {
			t.Error(found)
		}
	}
}

// The walker catches the shapes that slipped past its first version (RW1-G-03): the text kept in a variable first,
// formatted by fmt, and written through a ResponseWriter of any name.
func TestTheEchoWalkerCatchesIndirectShapes(t *testing.T) {
	src := `package x
func a(w http.ResponseWriter, err error) { msg := err.Error(); http.Error(w, msg, 500) }
func b(w http.ResponseWriter, err error) { jsonStatusError(w, 400, fmt.Sprint(err)) }
func c(w http.ResponseWriter, err error) { jsonStatusError(w, 400, fmt.Sprintf("failed: %v", err)) }
func d(rw http.ResponseWriter, err error) { rw.Write([]byte(err.Error())) }
func e(rw http.ResponseWriter, saveErr error) { fmt.Fprintf(rw, "no: %v", saveErr) }
func f(out http.ResponseWriter, err error) { _ = json.NewEncoder(out).Encode(map[string]string{"error": err.Error()}) }
func ok(w http.ResponseWriter, err error) { log.Print(err.Error()); http.Error(w, "internal server error", 500); jsonStatusError(w, 400, refusal(err)) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := echoesIn(fset, f)
	for _, fn := range []string{"x.go:2", "x.go:3", "x.go:4", "x.go:5", "x.go:6", "x.go:7"} {
		hit := false
		for _, s := range found {
			hit = hit || strings.HasPrefix(s, fn+":")
		}
		if !hit {
			t.Errorf("%s: not caught (found %v)", fn, found)
		}
	}
	for _, s := range found {
		if strings.HasPrefix(s, "x.go:8:") {
			t.Errorf("a fixed message or a named helper is not an echo: %s", s)
		}
	}
}

// echoesIn lists, in file f, every response write that is given an error's text: a call to http.Error,
// jsonStatusError or jsonResponse, the Write of a ResponseWriter parameter (any name), fmt.Fprint* to one, or a JSON
// encoder over one. In what such a call is given it flags X.Error(), a variable assigned from one in the same
// function, and an error-named variable (err, or ending in err/Err) formatted by fmt.Sprint*, fmt.Errorf or the
// Fprint itself. Without type information it is a net, not a proof: names do the work.
func echoesIn(fset *token.FileSet, f *ast.File) []string {
	var found []string
	named := map[string]bool{"http.Error": true, "jsonStatusError": true, "jsonResponse": true}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		writers := map[string]bool{}
		tainted := map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncType:
				if v.Params != nil {
					for _, field := range v.Params.List {
						if exprName(field.Type) == "http.ResponseWriter" {
							for _, name := range field.Names {
								writers[name.Name] = true
							}
						}
					}
				}
			case *ast.AssignStmt:
				for i, rhs := range v.Rhs {
					if i < len(v.Lhs) && hasErrorCall(rhs) {
						if id, ok := v.Lhs[i].(*ast.Ident); ok {
							tainted[id.Name] = true
						}
					}
				}
			case *ast.ValueSpec:
				for i, val := range v.Values {
					if i < len(v.Names) && hasErrorCall(val) {
						tainted[v.Names[i].Name] = true
					}
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := exprName(call.Fun)
			args := call.Args
			fprint := false
			switch {
			case named[name]:
			case isWriterMethod(call.Fun, "Write", writers):
			case (name == "fmt.Fprint" || name == "fmt.Fprintf" || name == "fmt.Fprintln") && len(args) > 0 && writers[exprName(args[0])]:
				args, fprint = args[1:], true
			case isEncodeOver(call.Fun, writers):
			default:
				return true
			}
			for _, arg := range args {
				if fprint && errorNamed(arg) {
					found = append(found, fmt.Sprintf("%s: %s formats %s into the response", fset.Position(arg.Pos()), name, exprName(arg)))
				}
				ast.Inspect(arg, func(m ast.Node) bool {
					switch v := m.(type) {
					case *ast.CallExpr:
						if errorText(v) {
							found = append(found, fmt.Sprintf("%s: %s sends %s.Error() to the client", fset.Position(v.Pos()), name, exprName(v.Fun.(*ast.SelectorExpr).X)))
						}
						if inner := exprName(v.Fun); strings.HasPrefix(inner, "fmt.Sprint") || inner == "fmt.Errorf" {
							for _, a := range v.Args {
								if errorNamed(a) {
									found = append(found, fmt.Sprintf("%s: %s formats %s into the response", fset.Position(a.Pos()), name, exprName(a)))
								}
							}
						}
					case *ast.Ident:
						if tainted[v.Name] {
							found = append(found, fmt.Sprintf("%s: %s sends %s, which holds an error's text", fset.Position(v.Pos()), name, v.Name))
						}
					}
					return true
				})
			}
			return true
		})
	}
	return found
}

// errorText reports whether call is X.Error() for an error whose words are not this code's own: a package's sentinel
// (mailtrigger.ErrAuth) has a fixed text and is not an echo.
func errorText(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Error" || len(call.Args) != 0 {
		return false
	}
	if recv, ok := sel.X.(*ast.SelectorExpr); ok {
		if _, pkg := recv.X.(*ast.Ident); pkg && strings.HasPrefix(recv.Sel.Name, "Err") {
			return false
		}
	}
	return true
}

// hasErrorCall reports whether e calls X.Error() (errorText) anywhere in it.
func hasErrorCall(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && errorText(call) {
			found = true
		}
		return !found
	})
	return found
}

// errorNamed reports whether e is a variable named like an error: err, or ending in err or Err.
func errorNamed(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && (strings.HasSuffix(id.Name, "err") || strings.HasSuffix(id.Name, "Err"))
}

// isWriterMethod reports whether fun is method on one of the ResponseWriter parameters writers.
func isWriterMethod(fun ast.Expr, method string, writers map[string]bool) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == method && writers[exprName(sel.X)]
}

// isEncodeOver reports whether fun is json.NewEncoder(w).Encode for a ResponseWriter parameter w.
func isEncodeOver(fun ast.Expr, writers map[string]bool) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Encode" {
		return false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	return ok && exprName(inner.Fun) == "json.NewEncoder" && len(inner.Args) == 1 && writers[exprName(inner.Args[0])]
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
