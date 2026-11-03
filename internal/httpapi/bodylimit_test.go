package httpapi

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No handler reads a request body unbounded (A-03): every function in this package, and in the packages whose
// handlers it mounts, that decodes or reads r.Body caps it in that same function (http.MaxBytesReader, limitJSON,
// limitUpload or io.LimitReader). A new handler that forgets fails here, not on an 8 GB host under a large post.
func TestEveryBodyReadIsCapped(t *testing.T) {
	// internal/practice's broker caps every route in the handler that mounts them (broker.go, maxBrokerBody).
	dirs := []string{".", "../handoff", "../knowledge", "../skillhub", "../workqueue", "../access"}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				body := string(src[fn.Body.Pos()-1 : fn.Body.End()-1])
				reads := strings.Contains(body, "json.NewDecoder(r.Body)") || strings.Contains(body, "io.ReadAll(r.Body)")
				capped := strings.Contains(body, "MaxBytesReader") || strings.Contains(body, "limitJSON(") ||
					strings.Contains(body, "limitUpload(") || strings.Contains(body, "LimitReader(r.Body")
				if reads && !capped {
					t.Errorf("%s: %s reads r.Body with no cap", file, fn.Name.Name)
				}
			}
		}
	}
}

// An over-limit body to each small JSON route is refused with 413, before anything is acted on.
func TestAnOverLimitJSONBodyIsRefused(t *testing.T) {
	s := &Server{}
	big := `{"x":"` + strings.Repeat("a", maxJSONBody+1) + `"}`
	for name, h := range map[string]http.HandlerFunc{
		"persona desktop": s.createPersonaDesktopHandler,
		"team deploy":     s.deployTeamHandler,
		"catalog install": s.installCatalogSkillHandler,
		"resource report": s.resourceReportHandler,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(big))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: %d %s, want 413", name, rec.Code, rec.Body)
		}
	}
}
