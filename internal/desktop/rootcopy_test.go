package desktop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// rootSideCopies are the only functions allowed to call Docker's root-side copy (CopyFromContainer /
// CopyToContainer). Each touches a fixed root-owned path, a container that has never run, or (missingapps) a direct
// child of the home, and says why where it is. Everything else that moves files in or out of a desktop's home goes
// through homefs.go, as the worker (B-01).
var rootSideCopies = []string{
	"claudelogin.go:readFileFromContainer", // the login container's fixed credentials path
	"missingapps.go:copyMissingFile",       // a direct child of the home
	"practice.go:CreatePractice",           // a practice desktop that has never started
	"practice.go:appendBrowserFlags",       // /opt, before the practice desktop starts
	"practice.go:copyOut",                  // fixed root-owned paths, and the frozen practice desktop's collection
	"practice.go:restoreSnapshot",          // a practice desktop that has never started
	"workerexec.go:putGateKey",             // the gate key, before the first start
}

// TestRootSideCopiesStayWhereTheyAreAllowed walks the package's sources for calls to Docker's root-side copy and
// fails on any outside rootSideCopies: a new one would be a new way for root to follow the worker's links.
func TestRootSideCopiesStayWhereTheyAreAllowed(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && (sel.Sel.Name == "CopyFromContainer" || sel.Sel.Name == "CopyToContainer") {
					found = append(found, file+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	allowed := map[string]bool{}
	for _, a := range rootSideCopies {
		allowed[a] = true
	}
	sort.Strings(found)
	for _, f := range found {
		if !allowed[f] {
			t.Errorf("%s calls Docker's root-side copy: move it to homefs.go (ReadTar/WriteTar), or, if the path is "+
				"root-owned or the container has never run, add it to rootSideCopies with the reason", f)
		}
	}
	if len(found) == 0 {
		t.Fatal("found no copy calls at all: the walk is broken")
	}
	seen := map[string]bool{}
	for _, f := range found {
		seen[f] = true
	}
	for _, a := range rootSideCopies {
		if !seen[a] {
			t.Errorf("%s no longer copies as root: take it off rootSideCopies, so it cannot come back unnoticed", a)
		}
	}
}
