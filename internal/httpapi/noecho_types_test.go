package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The no-echo rule by type, not by name (RW1-G-06 rest): TestNoResponseEchoesAnError reads names, so an error kept in
// a variable named like anything else, or a helper that hands back an error's text, got past it. This walk type-checks
// the package (export data of its imports from go list -export, as the desk-net test does) and fails on any value
// that reaches a response writer and is an error (its type implements error), holds an error's text (X.Error() of a
// non-sentinel, a local assigned from such text, fmt or strings over one), or comes from a function of this package
// whose result is derived from one. A response writer is anything whose type implements http.ResponseWriter: a call
// given one (http.Error, jsonStatusError, any helper) is checked on its arguments that are not themselves errors (a
// parameter of an error type is the helper's to log, and the helper's own body is walked too), and so is a Write or a
// json.NewEncoder(...).Encode on one. A package-level error (a sentinel: fixed words) is not an echo; refusal,
// providerSaid and viewerReason are the named doors for words meant for the person (json.go).
func TestNoResponseEchoesAnErrorByType(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks the package")
	}
	files, err := packageGoFiles(t)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
	}
	found, sinks := typedEchoes(t, fset, parsed)
	for _, s := range found {
		t.Error(s)
	}
	if sinks < 300 {
		t.Fatalf("only %d response writes were checked: the walk did not reach the handlers", sinks)
	}
}

// The typed walk catches what names could not: an error variable named like anything else, a helper that returns an
// error's text (directly or through another helper), strings over the text, and an error value given whole; and it
// lets through a comparison, a sentinel, a named door and an error given to a parameter of an error type.
func TestTheTypedEchoWalkerCatchesUnnamedErrorsAndHelpers(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks a sample")
	}
	src := `package x

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var errFixed = errors.New("fixed words")

type poll struct{ Problem error }

func refusal(err error) string    { return err.Error() }
func describe(e error) string     { return "failed: " + e.Error() }
func describeTwice(e error) string { return strings.TrimSpace(describe(e)) }
func kind(e error) string {
	if errors.Is(e, errFixed) {
		return "fixed"
	}
	return "other"
}
func writeMsg(w http.ResponseWriter, msg string) { http.Error(w, msg, 500) }
func logged(w http.ResponseWriter, why string, e error) { http.Error(w, why, 500) }

func a(w http.ResponseWriter, problem error) { http.Error(w, problem.Error(), 500) }
func b(w http.ResponseWriter, problem error) { writeMsg(w, describe(problem)) }
func c(w http.ResponseWriter, problem error) { writeMsg(w, describeTwice(problem)) }
func d(w http.ResponseWriter, problem error) { text := strings.ToUpper(problem.Error()); writeMsg(w, text) }
func e(w http.ResponseWriter, problem error) { _ = json.NewEncoder(w).Encode(map[string]any{"error": problem}) }
func f(w http.ResponseWriter, p poll) { fmt.Fprintf(w, "no: %v", p.Problem) }
func g(w http.ResponseWriter, problem error) { w.Write([]byte(fmt.Sprint(problem))) }
func ok(w http.ResponseWriter, problem error) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": problem == nil, "kind": kind(problem), "fixed": errors.Is(problem, errFixed)})
	writeMsg(w, refusal(problem))
	writeMsg(w, errFixed.Error())
	logged(w, "internal server error", problem)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, _ := typedEchoes(t, fset, []*ast.File{f})
	lines := map[int]bool{}
	for _, s := range found {
		var line int
		if _, err := fmt.Sscanf(strings.TrimPrefix(s, "x.go:"), "%d", &line); err == nil {
			lines[line] = true
		}
	}
	for fn, line := range map[string]int{"a": 27, "b": 28, "c": 29, "d": 30, "e": 31, "f": 32, "g": 33} {
		if !lines[line] {
			t.Errorf("%s (line %d): not caught (found %v)", fn, line, found)
		}
	}
	for line := 34; line <= 39; line++ {
		if lines[line] {
			t.Errorf("line %d is not an echo (found %v)", line, found)
		}
	}
	for line := 15; line <= 26; line++ {
		if lines[line] {
			t.Errorf("line %d (a helper or a write of a parameter) is not an echo itself (found %v)", line, found)
		}
	}
}

// packageGoFiles is this package's non-test Go files, as the build sees them on this system.
func packageGoFiles(t *testing.T) ([]string, error) {
	cmd := exec.Command("go", "list", "-json=GoFiles", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, stderr.String())
	}
	var p struct{ GoFiles []string }
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, err
	}
	return p.GoFiles, nil
}

var (
	exportsOnce sync.Once
	exportsMap  map[string]string
	exportsErr  error
)

// packageExports maps every import path this package depends on to its export data (go list -export -deps).
func packageExports() (map[string]string, error) {
	exportsOnce.Do(func() {
		cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Export", ".")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			exportsErr = fmt.Errorf("go list -export: %v: %s", err, stderr.String())
			return
		}
		exportsMap = map[string]string{}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p struct{ ImportPath, Export string }
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				exportsErr = err
				return
			}
			exportsMap[p.ImportPath] = p.Export
		}
	})
	return exportsMap, exportsErr
}

// echoNamedDoors are the functions of this package that may be given an error for its words: their names say what
// they may be given (json.go).
var echoNamedDoors = map[string]bool{"refusal": true, "refusalLines": true, "providerSaid": true, "viewerReason": true}

// echoForeignDoors are the functions of other packages of this module that answer an error with words chosen for the
// person (a sentence the package made, else a fixed line; the detail logged).
var echoForeignDoors = map[string]bool{"github.com/YKoustubhRao/decurix/internal/practice.Public": true}

// echoChecker holds one type-checked package and what the walk learned of it.
type echoChecker struct {
	fset    *token.FileSet
	pkg     *types.Package
	info    *types.Info
	errType *types.Interface
	writer  *types.Interface
	// tainted is every function of the package one of whose non-error results is derived from an error's text.
	tainted map[*types.Func]bool
	decls   map[*types.Func]*ast.FuncDecl
}

// typedEchoes type-checks files as one package and lists every echo in them, and how many response writes it checked.
func typedEchoes(t *testing.T, fset *token.FileSet, files []*ast.File) (found []string, sinks int) {
	t.Helper()
	exports, err := packageExports()
	if err != nil {
		t.Fatal(err)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{},
		Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: imp, Error: func(error) {}}
	pkg, err := conf.Check(files[0].Name.Name, fset, files, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	c := &echoChecker{fset: fset, pkg: pkg, info: info, tainted: map[*types.Func]bool{}, decls: map[*types.Func]*ast.FuncDecl{},
		errType: types.Universe.Lookup("error").Type().Underlying().(*types.Interface)}
	for _, p := range pkg.Imports() {
		if p.Path() == "net/http" {
			c.writer = p.Scope().Lookup("ResponseWriter").Type().Underlying().(*types.Interface)
		}
	}
	if c.writer == nil {
		t.Fatal("the package does not import net/http")
	}
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				if obj, ok := info.Defs[fn.Name].(*types.Func); ok {
					c.decls[obj] = fn
				}
			}
		}
	}
	c.findTaintedHelpers()

	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			local := c.localTaint(fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				args, isSink := c.sinkArgs(call)
				if !isSink {
					return true
				}
				sinks++
				for _, arg := range args {
					if why := c.leak(arg, local); why != "" {
						found = append(found, fmt.Sprintf("%s: %s reaches the response: %s", fset.Position(arg.Pos()), exprName(call.Fun), why))
					}
				}
				return true
			})
		}
	}
	sort.Strings(found)
	return found, sinks
}

// findTaintedHelpers marks, until nothing changes, every function of the package that returns an error's text in a
// non-error result. The named doors are not marked: that is what they are for.
func (c *echoChecker) findTaintedHelpers() {
	for changed := true; changed; {
		changed = false
		for obj, fn := range c.decls {
			if c.tainted[obj] || echoNamedDoors[obj.Name()] && obj.Pkg() == c.pkg {
				continue
			}
			results := obj.Type().(*types.Signature).Results()
			if results.Len() == 0 {
				continue
			}
			local := c.localTaint(fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				ret, ok := n.(*ast.ReturnStmt)
				if !ok || c.tainted[obj] {
					return !c.tainted[obj]
				}
				for i, r := range ret.Results {
					if len(ret.Results) == results.Len() && c.isError(results.At(i).Type()) {
						continue
					}
					if c.leak(r, local) != "" {
						c.tainted[obj], changed = true, true
					}
				}
				return true
			})
		}
	}
}

// localTaint is every variable in body assigned an error's text (a leak), in source order, twice over so a chain of
// assignments is followed.
func (c *echoChecker) localTaint(body *ast.BlockStmt) map[types.Object]bool {
	local := map[types.Object]bool{}
	mark := func(lhs ast.Expr) {
		id, ok := lhs.(*ast.Ident)
		if !ok {
			return
		}
		obj := c.info.Defs[id]
		if obj == nil {
			obj = c.info.Uses[id]
		}
		if obj != nil && !c.isError(obj.Type()) {
			local[obj] = true
		}
	}
	for pass := 0; pass < 2; pass++ {
		ast.Inspect(body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.AssignStmt:
				if len(v.Lhs) == len(v.Rhs) {
					for i := range v.Rhs {
						if c.leak(v.Rhs[i], local) != "" {
							mark(v.Lhs[i])
						}
					}
				} else if len(v.Rhs) == 1 && c.leak(v.Rhs[0], local) != "" {
					for _, l := range v.Lhs {
						mark(l)
					}
				}
			case *ast.ValueSpec:
				for i, val := range v.Values {
					if i < len(v.Names) && c.leak(val, local) != "" {
						mark(v.Names[i])
					}
				}
			}
			return true
		})
	}
	return local
}

// sinkArgs reports whether call writes a response, and the arguments it writes: a call given a response writer (its
// arguments for parameters that are not of an error type, nor the writer itself), a Write on one, or json.NewEncoder
// over one's Encode.
func (c *echoChecker) sinkArgs(call *ast.CallExpr) ([]ast.Expr, bool) {
	if tv, ok := c.info.Types[call.Fun]; ok && tv.IsType() {
		return nil, false
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if (sel.Sel.Name == "Write" || sel.Sel.Name == "WriteString") && c.isWriter(c.info.TypeOf(sel.X)) {
			return call.Args, true
		}
		if inner, ok := sel.X.(*ast.CallExpr); ok && sel.Sel.Name == "Encode" && exprName(inner.Fun) == "json.NewEncoder" &&
			len(inner.Args) == 1 && c.isWriter(c.info.TypeOf(inner.Args[0])) {
			return call.Args, true
		}
	}
	sig, ok := c.info.TypeOf(call.Fun).(*types.Signature)
	if !ok {
		return nil, false
	}
	givenWriter := false
	for _, arg := range call.Args {
		if c.isWriter(c.info.TypeOf(arg)) {
			givenWriter = true
		}
	}
	if !givenWriter {
		return nil, false
	}
	var out []ast.Expr
	for i, arg := range call.Args {
		var param types.Type
		switch {
		case sig.Variadic() && i >= sig.Params().Len()-1:
			param = sig.Params().At(sig.Params().Len() - 1).Type()
			if s, ok := param.(*types.Slice); ok && !call.Ellipsis.IsValid() {
				param = s.Elem()
			}
		case i < sig.Params().Len():
			param = sig.Params().At(i).Type()
		}
		if param == nil || c.isError(param) || c.isWriter(c.info.TypeOf(arg)) {
			continue
		}
		out = append(out, arg)
	}
	return out, true
}

// leak says how e carries an error's words, "" when it does not.
func (c *echoChecker) leak(e ast.Expr, local map[types.Object]bool) string {
	tv := c.info.Types[e]
	if tv.Value != nil {
		return ""
	}
	if b, ok := types.Unalias(tv.Type).Underlying().(*types.Basic); ok && b.Info()&(types.IsBoolean|types.IsNumeric) != 0 {
		return ""
	}
	switch v := e.(type) {
	case *ast.Ident:
		obj := c.info.Uses[v]
		if obj != nil && local[obj] {
			return v.Name + " holds an error's text"
		}
		if c.isError(tv.Type) && !c.sentinel(obj) {
			return v.Name + " is an error"
		}
	case *ast.SelectorExpr:
		if c.isError(tv.Type) && !c.sentinel(c.info.Uses[v.Sel]) {
			return exprName(v) + " is an error"
		}
	case *ast.ParenExpr:
		return c.leak(v.X, local)
	case *ast.StarExpr:
		return c.leak(v.X, local)
	case *ast.UnaryExpr:
		return c.leak(v.X, local)
	case *ast.SliceExpr:
		return c.leak(v.X, local)
	case *ast.TypeAssertExpr:
		return c.leak(v.X, local)
	case *ast.BinaryExpr:
		if why := c.leak(v.X, local); why != "" {
			return why
		}
		return c.leak(v.Y, local)
	case *ast.CompositeLit:
		for _, elt := range v.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			if why := c.leak(elt, local); why != "" {
				return why
			}
		}
	case *ast.CallExpr:
		return c.callLeak(v, tv, local)
	}
	return ""
}

// callLeak says how a call's result carries an error's words: X.Error() of an error that is not a sentinel, a
// function of this package that returns such text, a conversion of it, or a call outside the package whose result
// can carry text (a string, bytes, an error or an interface) given it.
func (c *echoChecker) callLeak(call *ast.CallExpr, tv types.TypeAndValue, local map[types.Object]bool) string {
	if ft, ok := c.info.Types[call.Fun]; ok && ft.IsType() {
		for _, a := range call.Args {
			if why := c.leak(a, local); why != "" {
				return why
			}
		}
		return ""
	}
	var callee types.Object
	switch f := call.Fun.(type) {
	case *ast.Ident:
		callee = c.info.Uses[f]
	case *ast.SelectorExpr:
		callee = c.info.Uses[f.Sel]
		if f.Sel.Name == "Error" && len(call.Args) == 0 && c.isError(c.info.TypeOf(f.X)) {
			if x, ok := f.X.(*ast.Ident); ok && c.sentinel(c.info.Uses[x]) {
				return ""
			}
			if x, ok := f.X.(*ast.SelectorExpr); ok && c.sentinel(c.info.Uses[x.Sel]) {
				return ""
			}
			return exprName(f.X) + ".Error()"
		}
	case *ast.FuncLit:
		return ""
	}
	switch fn := callee.(type) {
	case *types.Builtin:
		if fn.Name() != "append" {
			return ""
		}
	case *types.Func:
		if fn.Pkg() == c.pkg {
			if echoNamedDoors[fn.Name()] {
				return ""
			}
			if c.tainted[fn] {
				return fn.Name() + " returns an error's text"
			}
			return ""
		}
		if echoForeignDoors[fn.FullName()] {
			return ""
		}
	case nil:
	default:
		return "" // a func-typed variable or field: what it returns is its own
	}
	if c.isError(tv.Type) {
		return exprName(call.Fun) + " makes an error"
	}
	if !c.carriesText(tv.Type) {
		return ""
	}
	for _, a := range call.Args {
		if why := c.leak(a, local); why != "" {
			return why
		}
	}
	return ""
}

// carriesText reports whether a value of t can hold an error's words: a string, bytes, or an interface (a tuple: any
// of its parts).
func (c *echoChecker) carriesText(t types.Type) bool {
	if t == nil {
		return false
	}
	if tup, ok := t.(*types.Tuple); ok {
		for i := 0; i < tup.Len(); i++ {
			if c.carriesText(tup.At(i).Type()) {
				return true
			}
		}
		return false
	}
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString != 0
	case *types.Slice:
		b, ok := u.Elem().Underlying().(*types.Basic)
		return ok && b.Kind() == types.Byte
	case *types.Interface:
		return true
	}
	return false
}

// isError reports whether t is an error: its type implements error (an untyped nil is not one).
func (c *echoChecker) isError(t types.Type) bool {
	if t == nil {
		return false
	}
	if b, ok := t.(*types.Basic); ok && b.Kind() == types.UntypedNil {
		return false
	}
	return types.Implements(t, c.errType)
}

// isWriter reports whether t is a response writer: its type implements http.ResponseWriter.
func (c *echoChecker) isWriter(t types.Type) bool {
	if t == nil {
		return false
	}
	if _, ok := types.Unalias(t).Underlying().(*types.Interface); ok {
		return types.Implements(t, c.writer) && !types.Identical(t.Underlying(), types.NewInterfaceType(nil, nil))
	}
	return types.Implements(t, c.writer)
}

// sentinel reports whether obj is a package-level variable (errors.New at a package's top: fixed words).
func (c *echoChecker) sentinel(obj types.Object) bool {
	v, ok := obj.(*types.Var)
	return ok && !v.IsField() && v.Pkg() != nil && v.Parent() == v.Pkg().Scope()
}
