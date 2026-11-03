package eval

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repository's root, two folders up from this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// suiteNames lists every name a dev suite gives itself: suite names, case ids and mock names.
func suiteNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	suites, err := os.ReadDir(filepath.Join(root, "evals", "suites"))
	if err != nil {
		t.Logf("no evals/suites here (%v): checking the path only", err)
		return nil
	}
	for _, s := range suites {
		if !s.IsDir() {
			continue
		}
		names = append(names, s.Name())
		raw, err := os.ReadFile(filepath.Join(root, "evals", "suites", s.Name(), "suite.json"))
		if err != nil {
			t.Fatalf("%s: %v", s.Name(), err)
		}
		var doc Suite
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s/suite.json: %v", s.Name(), err)
		}
		names = append(names, doc.Name)
		names = append(names, doc.CaseIDs...)
		// Mock hosts are generic example names (RFC 2606 .test), which docs and other tests may use as examples too.
		for m := range doc.Mocks {
			names = append(names, m)
		}
	}
	return names
}

// TestHarnessNeverNamesSuites is the dev-suite boundary (D7): no code or text under agent/ or internal/ (apart from
// this package and its routes) reads evals/ or names a suite, a case or a suite's mock. Harness code
// that knew a case could be fitted to it.
func TestHarnessNeverNamesSuites(t *testing.T) {
	root := repoRoot(t)
	forbidden := append([]string{"evals/", `evals\`}, suiteNames(t, root)...)
	textExt := map[string]bool{".go": true, ".py": true, ".md": true, ".json": true, ".js": true, ".html": true,
		".sh": true, ".ps1": true, ".txt": true, ".css": true, ".tmpl": true, ".yaml": true, ".yml": true, ".toml": true}
	checked := 0
	for _, top := range []string{"agent", "internal"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				switch d.Name() {
				case "__pycache__", "node_modules", ".git":
					return filepath.SkipDir
				}
				if rel == "internal/eval" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(rel, "internal/httpapi/eval_routes") || !textExt[strings.ToLower(filepath.Ext(p))] {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			checked++
			text := string(b)
			for _, name := range forbidden {
				if name != "" && strings.Contains(text, name) {
					t.Errorf("%s names %q: harness code never reads evals/ or names a suite, case or mock", rel, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 100 {
		t.Fatalf("walked only %d files; the boundary test is not looking where the harness is", checked)
	}
}
