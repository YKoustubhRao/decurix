package decurix

// Schema steps are append-only (CLAUDE.md, "SQLite"): a step that has shipped has run on someone's database, and
// editing it changes nothing there while new installs get the edited one - two schemas under one version. This test
// reads every component's steps from the source (each sqlitedb.Migrate call and the []string it is given) and checks
// them against internal/sqlitedb/shipped_steps.json: an edited or removed step fails; a new one fails until it is
// recorded, with DECURIX_RECORD_STEPS=1. Recording only ever appends. (After agent-orchestrator's ledger of shipped
// migrations, kept after renumbered versions broke their session list.)

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// shippedStepsFile is the record of every component's schema steps, as hashes.
const shippedStepsFile = "internal/sqlitedb/shipped_steps.json"

func TestSchemaStepsAreAppendOnly(t *testing.T) {
	current, err := schemaSteps("internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(current) == 0 {
		t.Fatal("no sqlitedb.Migrate call found: the reader no longer matches the code")
	}

	recorded := map[string][]string{}
	if raw, err := os.ReadFile(shippedStepsFile); err == nil {
		if err := json.Unmarshal(raw, &recorded); err != nil {
			t.Fatalf("%s: %v", shippedStepsFile, err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	record := os.Getenv("DECURIX_RECORD_STEPS") == "1"
	changed := false
	for component, steps := range current {
		shipped := recorded[component]
		for i, hash := range shipped {
			if i >= len(steps) {
				t.Errorf("component %q: step %d of %d was removed; a shipped step is never removed", component, i+1, len(shipped))
				break
			}
			if steps[i] != hash {
				t.Errorf("component %q: step %d was edited after it shipped; add a new step at the end instead", component, i+1)
			}
		}
		if len(steps) > len(shipped) {
			if !record {
				t.Errorf("component %q has %d new step(s) not recorded: run DECURIX_RECORD_STEPS=1 go test -run "+
					"TestSchemaStepsAreAppendOnly . once they are final", component, len(steps)-len(shipped))
				continue
			}
			recorded[component] = append(append([]string{}, shipped...), steps[len(shipped):]...)
			changed = true
		}
	}
	for component := range recorded {
		if _, ok := current[component]; !ok {
			t.Errorf("component %q was recorded but its sqlitedb.Migrate call is gone; its tables still exist on every install", component)
		}
	}
	if changed && !t.Failed() {
		out, _ := json.MarshalIndent(sortedSteps(recorded), "", "  ")
		if err := os.WriteFile(shippedStepsFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded new steps in %s", shippedStepsFile)
	}
}

// sortedSteps is the record with its components in order, so the file's diff shows only what was appended.
func sortedSteps(steps map[string][]string) map[string][]string {
	keys := make([]string, 0, len(steps))
	for k := range steps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string][]string, len(steps))
	for _, k := range keys {
		out[k] = steps[k]
	}
	return out // encoding/json writes map keys sorted
}

// schemaSteps reads, under root, every sqlitedb.Migrate(db, "<component>", <var>) call outside tests and the hashes
// of the []string var it names, declared in the same package.
func schemaSteps(root string) (map[string][]string, error) {
	out := map[string][]string{}
	dirs := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			dirs[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for dir := range dirs {
		found, err := packageSteps(dir)
		if err != nil {
			return nil, err
		}
		for component, steps := range found {
			out[component] = steps
		}
	}
	return out, nil
}

// packageSteps is schemaSteps for one package directory.
func packageSteps(dir string) (map[string][]string, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sources := map[*ast.File][]byte{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			return nil, err
		}
		sources[file] = src
	}

	calls := map[string]string{} // component -> var name
	for file := range sources {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Migrate" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "sqlitedb" {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			name, ok2 := call.Args[2].(*ast.Ident)
			if !ok || !ok2 || lit.Kind != token.STRING {
				return true
			}
			component, err := strconv.Unquote(lit.Value)
			if err == nil {
				calls[component] = name.Name
			}
			return true
		})
	}

	out := map[string][]string{}
	for component, varName := range calls {
		for file, src := range sources {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, ident := range vs.Names {
						if ident.Name != varName || i >= len(vs.Values) {
							continue
						}
						list, ok := vs.Values[i].(*ast.CompositeLit)
						if !ok {
							continue
						}
						var steps []string
						for _, elt := range list.Elts {
							steps = append(steps, stepHash(fset, src, elt))
						}
						out[component] = steps
					}
				}
			}
		}
	}
	return out, nil
}

// stepHash is a step's sha256: of its text when it is a plain string literal, else of its source.
func stepHash(fset *token.FileSet, src []byte, elt ast.Expr) string {
	text := ""
	if lit, ok := elt.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if v, err := strconv.Unquote(lit.Value); err == nil {
			text = v
		}
	}
	if text == "" {
		start, end := fset.Position(elt.Pos()).Offset, fset.Position(elt.End()).Offset
		text = string(src[start:end])
	}
	sum := sha256.Sum256([]byte(strings.ReplaceAll(text, "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}
