package desktop

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// No address is read from a container's networks but through desktopEndpoint (desknet.go), so none reads the shared
// network, or any network, directly. By type, not by spelling (review NET-R3): every index of, and every range over,
// a map[string]*network.EndpointSettings - however it was reached, through an alias or a field - and every read of
// the legacy container NetworkSettings.IPAddress, in every non-test file of the control plane, outside desknet.go and
// the practice network's own endpoint (practice.go's practiceEndpoint). The packages are type-checked from the build cache's export data
// (go list -export), so it costs a build that has mostly happened already.
func TestNoAddressIsReadFromTheSharedNetworkDirectly(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks the control plane")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export,Module", "./internal/...", "./cmd/...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, stderr.String())
	}
	type pkg struct {
		ImportPath, Dir, Export string
		GoFiles                 []string
		Module                  *struct{ Path string }
	}
	exports := map[string]string{}
	var ours []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Path == "github.com/YKoustubhRao/decurix" &&
			(strings.Contains(p.ImportPath, "/internal/") || strings.Contains(p.ImportPath, "/cmd/")) {
			ours = append(ours, p)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})

	files, sawDesknet := 0, false
	for _, p := range ours {
		var parsed []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			parsed = append(parsed, f)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		conf := types.Config{Importer: imp, Error: func(error) {}}
		if _, err := conf.Check(p.ImportPath, fset, parsed, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, f := range parsed {
			files++
			pos := fset.Position(f.Pos())
			base, dir := filepath.Base(pos.Filename), filepath.Base(filepath.Dir(pos.Filename))
			if dir == "desktop" && base == "desknet.go" {
				sawDesknet = true
			}
			for _, decl := range f.Decls {
				// desknet.go is where addresses are read; of practice.go, only practiceEndpoint (the practice
				// network's own endpoint) may read one (review NET-R3-2).
				fn, _ := decl.(*ast.FuncDecl)
				allowed := dir == "desktop" && (base == "desknet.go" || base == "practice.go" && fn != nil && fn.Name.Name == "practiceEndpoint")
				ast.Inspect(decl, func(n ast.Node) bool {
					var bad ast.Node
					switch x := n.(type) {
					case *ast.IndexExpr:
						if isEndpointMap(info.TypeOf(x.X)) {
							bad = x
						}
					case *ast.RangeStmt:
						if isEndpointMap(info.TypeOf(x.X)) {
							bad = x
						}
					case *ast.SelectorExpr:
						if sel := info.Selections[x]; sel != nil && sel.Kind() == types.FieldVal && sel.Obj().Name() == "IPAddress" &&
							sel.Obj().Pkg() != nil && sel.Obj().Pkg().Path() == "github.com/docker/docker/api/types/container" {
							bad = x
						}
					}
					if bad != nil && !allowed {
						t.Errorf("%s: a container's address read outside desktopEndpoint", fset.Position(bad.Pos()))
					}
					return true
				})
			}
		}
	}
	if files < 50 || !sawDesknet {
		t.Fatalf("checked %d files (desknet.go seen: %v): the walk did not reach the control plane", files, sawDesknet)
	}
}

// isEndpointMap reports whether t is a container's map of network endpoints: map[string]*network.EndpointSettings.
func isEndpointMap(t types.Type) bool {
	m, ok := types.Unalias(t).(*types.Map)
	if !ok {
		return false
	}
	ptr, ok := m.Elem().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(ptr.Elem()).(*types.Named)
	return ok && named.Obj().Name() == "EndpointSettings" && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == "github.com/docker/docker/api/types/network"
}
