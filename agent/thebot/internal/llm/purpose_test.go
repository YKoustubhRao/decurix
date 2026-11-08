package llm

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// purposeConstants is every Purpose* constant declared in purpose.go, by name, with its value.
func purposeConstants(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "purpose.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && strings.HasPrefix(name.Name, "Purpose") {
					out[name.Name] = strings.Trim(lit.Value, `"`)
				}
			}
		}
	}
	return out
}

func TestPurposeConstantsAreValidNames(t *testing.T) {
	consts := purposeConstants(t)
	if len(consts) < 20 {
		t.Fatalf("found only %d Purpose constants in purpose.go", len(consts))
	}
	seen := map[string]string{}
	for name, value := range consts {
		if !purposeShape.MatchString(value) {
			t.Errorf("%s = %q does not match the proxy's purpose form %s", name, value, purposeShape)
		}
		if !ValidPurpose(value) {
			t.Errorf("%s = %q is not in the purposes set", name, value)
		}
		if other, dup := seen[value]; dup {
			t.Errorf("%s and %s are both %q", name, other, value)
		}
		seen[value] = name
	}
	if len(purposes) != len(consts) {
		t.Errorf("the purposes set has %d names, purpose.go declares %d constants", len(purposes), len(consts))
	}
}

// TestEveryOptionsLiteralNamesItsPurpose parses every non-test Go file of soul's internal/ and cmd/ and fails for an
// llm.Options literal (or Options inside package llm) with no Purpose key, or with a Purpose that is not one of
// purpose.go's constants.
func TestEveryOptionsLiteralNamesItsPurpose(t *testing.T) {
	consts := purposeConstants(t)
	root := filepath.Join("..", "..") // agent/thebot
	literals := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			inLLM := f.Name.Name == "llm"
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isOptionsType(lit.Type, inLLM) {
					return true
				}
				literals++
				where := fset.Position(lit.Pos()).String()
				var purpose ast.Expr
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Purpose" {
							purpose = kv.Value
						}
					}
				}
				if purpose == nil {
					t.Errorf("%s: an llm.Options literal sets no Purpose", where)
					return true
				}
				if agentPurposeCall(purpose) {
					return true // the Python agent's own purpose, checked by AgentPurpose (/api/llm/call)
				}
				if name := purposeConstName(purpose, inLLM); name == "" || consts[name] == "" {
					t.Errorf("%s: Purpose is not one of purpose.go's constants", where)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if literals < 20 {
		t.Fatalf("found only %d llm.Options literals: the walk is not seeing soul's sources", literals)
	}
}

// isOptionsType reports whether a composite literal's type is llm.Options (or Options inside package llm).
func isOptionsType(typ ast.Expr, inLLM bool) bool {
	switch x := typ.(type) {
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && pkg.Name == "llm" && x.Sel.Name == "Options"
	case *ast.Ident:
		return inLLM && x.Name == "Options"
	}
	return false
}

// purposeConstName is the constant's name when e is llm.PurposeX (or PurposeX inside package llm), else "".
func purposeConstName(e ast.Expr, inLLM bool) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "llm" {
			return x.Sel.Name
		}
	case *ast.Ident:
		if inLLM {
			return x.Name
		}
	}
	return ""
}

// agentPurposeCall reports whether e is a call of llm.AgentPurpose.
func agentPurposeCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "llm" && sel.Sel.Name == "AgentPurpose"
}

func TestAgentPurposeIsTheAgentsOwnOrUntagged(t *testing.T) {
	for p, want := range map[string]string{
		"py.judge": "py.judge", "py.escalation.ground": "py.escalation.ground", "legacy.x": "legacy.x",
		PurposeOperatorTurn: PurposeUntagged, "": PurposeUntagged, "py.": PurposeUntagged, "PY.judge": PurposeUntagged,
		"py.judge\nX-Evil: 1": PurposeUntagged, "py." + strings.Repeat("a", 45): PurposeUntagged,
	} {
		if got := AgentPurpose(p); got != want {
			t.Errorf("AgentPurpose(%q) = %q, want %q", p, got, want)
		}
	}
}

// recordingProvider keeps the options of each call it is given.
type recordingProvider struct{ opts []Options }

func (r *recordingProvider) Name() string                     { return "rec" }
func (r *recordingProvider) IsReachable(context.Context) bool { return true }
func (r *recordingProvider) Chat(_ context.Context, _ string, _ []Message, opts Options) (Result, error) {
	r.opts = append(r.opts, opts)
	return Result{Text: "ok"}, nil
}

func untaggedCall(p Provider) error {
	_, err := p.Chat(context.Background(), "m", nil, Options{})
	return err
}

func TestUntaggedCallGoesOutUntaggedAndIsLoggedOncePerSite(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	inner := &recordingProvider{}
	p := Metered(inner)
	for i := 0; i < 3; i++ {
		if err := untaggedCall(p); err != nil {
			t.Fatalf("an untagged call failed: %v", err)
		}
	}
	if _, err := p.Chat(context.Background(), "m", nil, Options{Purpose: PurposeReview}); err != nil {
		t.Fatal(err)
	}
	if len(inner.opts) != 4 || inner.opts[0].Purpose != PurposeUntagged || inner.opts[3].Purpose != PurposeReview {
		t.Fatalf("purposes passed on: %+v", inner.opts)
	}
	logged := buf.String()
	if n := strings.Count(logged, "names no purpose"); n != 1 {
		t.Fatalf("logged %d times, want once per call site:\n%s", n, logged)
	}
	if !strings.Contains(logged, "purpose_test.go:") {
		t.Fatalf("the log line does not name the caller's file:line: %s", logged)
	}
}
