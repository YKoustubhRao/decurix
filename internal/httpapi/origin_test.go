package httpapi

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// latestExecution is the task's latest execution as the records keep it.
func latestExecution(t *testing.T, s *Server, taskID string) tasks.Execution {
	t.Helper()
	d, err := s.Tasks.Get(taskID)
	if err != nil || len(d.Executions) == 0 {
		t.Fatalf("task %s: %+v, %v", taskID, d, err)
	}
	return d.Executions[len(d.Executions)-1]
}

// Every door writes what started the execution (plan R7): a person's chat send with its door and who sent it, the
// concierge, and an automated producer's own origin (a DAG node here; schedule, mail, queue and handoff set theirs
// the same way, which TestEveryProducerNamesItsOrigin checks).
func TestEveryDoorStoresItsOrigin(t *testing.T) {
	s := chatServer(t)
	fakeAgentDesktop(t, s)

	rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"count the bills"}, "door": {"card"}}, false, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	task := between(rec.Body.String(), `"task_id":"`, `"`)
	if o := latestExecution(t, s, task).Origin; o.Kind != tasks.OriginPerson || o.Ref != "card" || o.ActorID != tasks.LocalOwner.ID {
		t.Fatalf("a person's send = %+v", o)
	}
	_, _ = s.Tasks.RecordOutcome("box", task, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "12"})

	req := httptest.NewRequest(http.MethodPost, "/api/concierge/send", nil)
	req.RemoteAddr = "127.0.0.1:1"
	task, _, err := s.ConciergeDeps().Send(conciergeContext(s.ActorContext(req)), "box", "file the receipts")
	if err != nil {
		t.Fatal(err)
	}
	if o := latestExecution(t, s, task).Origin; o.Kind != tasks.OriginConcierge || o.ActorID != tasks.LocalOwner.ID {
		t.Fatalf("the concierge's send = %+v", o)
	}
	_, _ = s.Tasks.RecordOutcome("box", task, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "ok"})

	task, err = s.SubmitTracked(tasks.WithOrigin(context.Background(), tasks.Origin{Kind: tasks.OriginDAG, Ref: "r1/n2"}), "box", "next step")
	if err != nil {
		t.Fatal(err)
	}
	if o := latestExecution(t, s, task).Origin; o.Kind != tasks.OriginDAG || o.Ref != "r1/n2" || o.ActorID != "" {
		t.Fatalf("a DAG node's send = %+v", o)
	}
	_, _ = s.Tasks.RecordOutcome("box", task, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "ok"})

	// A producer that names none is stored as unknown, never as a person.
	task, err = s.SubmitTracked(context.Background(), "box", "unlabelled")
	if err != nil {
		t.Fatal(err)
	}
	if o := latestExecution(t, s, task).Origin; o.Kind != tasks.OriginUnknown {
		t.Fatalf("an unlabelled send = %+v", o)
	}
}

// between is the text of s between the first from and the next to.
func between(s, from, to string) string {
	_, rest, _ := strings.Cut(s, from)
	out, _, _ := strings.Cut(rest, to)
	return out
}

// Every automated producer names its origin on the context of its submit: the first argument of each submit call
// outside httpapi is tasks.WithOrigin(...), or a context the same function assigned from it.
func TestEveryProducerNamesItsOrigin(t *testing.T) {
	submits := map[string]bool{"Submit": true, "SubmitTracked": true, "SubmitTrackedWithFiles": true, "SubmitHandoff": true}
	found := 0
	for _, pkg := range []string{"schedule", "mailtrigger", "dag", "workqueue", "handoff"} {
		files, _ := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				labelled := map[string]bool{} // contexts this function assigned from tasks.WithOrigin
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 && isWithOrigin(as.Rhs[0]) {
						if id, ok := as.Lhs[0].(*ast.Ident); ok {
							labelled[id.Name] = true
						}
					}
					return true
				})
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || len(call.Args) == 0 {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !submits[sel.Sel.Name] {
						return true
					}
					if _, isType := sel.X.(*ast.Ident); !isType && !isField(sel.X) {
						return true
					}
					found++
					arg := call.Args[0]
					if id, ok := arg.(*ast.Ident); ok && labelled[id.Name] || isWithOrigin(arg) {
						return true
					}
					t.Errorf("%s: %s calls %s without naming its origin (tasks.WithOrigin)", file, fn.Name.Name, sel.Sel.Name)
					return true
				})
			}
		}
	}
	if found < 6 {
		t.Fatalf("only %d submit calls found: the walk is not reading the producers", found)
	}
}

// isWithOrigin reports whether e is a call to tasks.WithOrigin.
func isWithOrigin(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithOrigin" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "tasks"
}

// isField reports whether e is a field selection (r.Submitter, s.Submit).
func isField(e ast.Expr) bool {
	_, ok := e.(*ast.SelectorExpr)
	return ok
}

// Every caller that writes a task record directly is one of these, each of which sets the route's origin first; a
// new one has to be added here with where its origin comes from.
func TestEveryCommitCallerIsKnown(t *testing.T) {
	known := map[string]string{
		"httpapi/requests.go:recordDelivered":              "route.Origin set by submitStored / recordTaskHandler from the door",
		"httpapi/qualification.go:qualificationRunHandler": "practice, readiness/<run>",
		"practice/runner.go:*":                             "practice (EVAL lane)",
		"eval/runner.go:*":                                 "eval (EVAL lane)",
	}
	var got []string
	root := ".."
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "tasks/") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "Commit", "CommitTyped", "CommitReply":
				default:
					return true
				}
				if x, ok := sel.X.(*ast.SelectorExpr); !ok || x.Sel.Name != "Tasks" {
					return true // tx.Commit and the like
				}
				got = append(got, rel+":"+fn.Name.Name)
				return true
			})
		}
		return nil
	})
	sort.Strings(got)
	if len(got) < 3 {
		t.Fatalf("only %v found: the walk is not reading the callers", got)
	}
	for _, c := range got {
		file, _, _ := strings.Cut(c, ":")
		if known[c] == "" && known[file+":*"] == "" {
			t.Errorf("%s writes task records directly: set the route's origin and list it here", c)
		}
	}
}

func TestResolvedModelAndPassRates(t *testing.T) {
	s := recordingServer(t)
	w := tasks.Worker{ID: "box", Name: "box", Engine: "thebot", Model: "default"}
	route, _ := s.Tasks.Commit(tasks.NewRoute(), w, "count the bills")
	for _, c := range []struct{ dep, purpose string }{{"small-1", "review"}, {"small-1", "review"}, {"big-1", "operator.turn"}} {
		s.LLMRec.Record(context.Background(), llmrec.Call{Provider: "azure", Deployment: c.dep, Desktop: "box",
			Task: route.TaskID, Purpose: c.purpose, Request: []byte(`{"messages":[]}`), Status: 200, At: time.Now()})
	}
	finalWithVerdict(t, s, route.TaskID, "12 bills.", doneVerdict, true)
	if e := latestExecution(t, s, route.TaskID); e.ResolvedModel != "big-1" {
		t.Fatalf("resolved model = %q", e.ResolvedModel)
	}

	// A local brain's run (no recorded call) is named by the engine and the model the desktop said.
	local, _ := s.Tasks.Commit(tasks.NewRoute(), w, "count again")
	body := `{"desktop_id":"box","kind":"final","content":"12","task_id":"` + local.TaskID + `","engine":"thebot","model":"qwen3:8b","checked":true,` +
		`"skill":{"name":"bill-count","version":3}}`
	req := httptest.NewRequest(http.MethodPost, "/api/querytext", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), callerKey{}, "box"))
	rr := httptest.NewRecorder()
	s.queryTextHandler(rr, req)
	e := latestExecution(t, s, local.TaskID)
	if e.ResolvedModel != "thebot:qwen3:8b" || e.Skill.Name != "bill-count" || e.Skill.Version != 3 {
		t.Fatalf("a local run = %+v", e)
	}
	if _, err := s.Tasks.SetVerdict(local.TaskID, tasks.VerdictWrong, "", tasks.LocalOwner); err != nil {
		t.Fatal(err)
	}

	rr = httptest.NewRecorder()
	s.passRatesHandler(rr, httptest.NewRequest(http.MethodGet, "/api/tasks/pass-rates", nil))
	out := rr.Body.String()
	for _, want := range []string{`"skill_name":"bill-count","skill_version":3,"model":"thebot:qwen3:8b","runs":1,"checked_done":1,"person_correct":0,"person_wrong":1,"wrong_but_checked":1`,
		`"model":"big-1","runs":1,"checked_done":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("pass rates lack %s:\n%s", want, out)
		}
	}
}
