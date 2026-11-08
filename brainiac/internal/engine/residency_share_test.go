package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/store"
)

// twoModelLock is a plan of two fake llama-servers, a brain and an embedder,
// whose model files are the given paths.
func twoModelLock(t *testing.T, brainFile, embedFile string) *install.Lock {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srcA := catalog.Source{Kind: catalog.SourceHF, Runtime: catalog.RuntimeLlamaCpp, Repo: "fake/a", Quant: "Q4"}
	srcB := catalog.Source{Kind: catalog.SourceHF, Runtime: catalog.RuntimeLlamaCpp, Repo: "fake/b", Quant: "Q4"}
	return &install.Lock{LlamaServer: self, Plan: &selector.Plan{
		Picks: []selector.Pick{
			{Role: catalog.RoleBrain, Model: "a", Pool: "host", MemGB: 0.1, Source: srcA},
			{Role: catalog.RoleEmbed, Model: "b", Pool: "host", MemGB: 0.1, Source: srcB},
		},
		Budgets: []selector.Budget{{Pool: "host", BudgetGB: 100}}},
		Units: map[string]install.Unit{
			install.UnitKey("a", srcA): {Files: map[string]string{"model": brainFile}},
			install.UnitKey("b", srcB): {Files: map[string]string{"model": embedFile}},
		}}
}

func modelFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("serve"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sharedEngine(t *testing.T, st *store.Store, res *Residency, lock *install.Lock) *Engine {
	t.Helper()
	cfg := config.Default()
	cfg.Mode, cfg.Sandbox = config.ModeLocal, config.SandboxOff
	e, err := New(Options{Config: cfg, Store: st, Lock: lock, Residency: res})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// G-11: an engine built for a changed plan on the server's residency keeps
// the model server whose recipe did not change running (same process, no
// load), and the engine it replaced stops only the one that changed.
func TestANewEngineKeepsTheUnchangedModelRunning(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := NewServerResidency(nil)
	dir := t.TempDir()
	brain, embedOld, embedNew := modelFile(t, dir, "a.gguf"), modelFile(t, dir, "b-old.gguf"), modelFile(t, dir, "b-new.gguf")

	old := sharedEngine(t, st, res, twoModelLock(t, brain, embedOld))
	ctx := context.Background()
	oa, ob := old.units[keyOf(old, "a")], old.units[keyOf(old, "b")]
	for _, u := range []*unit{oa, ob} {
		rel, ms, err := old.res.Acquire(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if ms == 0 {
			t.Fatalf("%s was not loaded", u.model)
		}
		rel()
	}
	if !oa.llama.Running() || !ob.llama.Running() {
		t.Fatal("the old plan's servers are not running")
	}

	// The embedder's file changed; the brain did not.
	next := sharedEngine(t, st, res, twoModelLock(t, brain, embedNew))
	na, nb := next.units[keyOf(next, "a")], next.units[keyOf(next, "b")]
	if na.llama != oa.llama {
		t.Fatal("the unchanged brain got a new model server")
	}
	if nb.llama == ob.llama || nb.llama.Running() {
		t.Fatal("the changed embedder reuses the old process")
	}
	old.Retire(nil)
	if !na.llama.Running() {
		t.Fatal("retiring the old engine stopped the model the new one kept")
	}
	if ob.llama.Running() {
		t.Fatal("the changed model's old server still runs after its engine retired")
	}
	rel, ms, err := next.res.Acquire(ctx, na)
	if err != nil || ms != 0 {
		t.Fatalf("the kept brain loaded again (%d ms, %v)", ms, err)
	}
	rel()
	rel, ms, err = next.res.Acquire(ctx, nb)
	if err != nil || ms == 0 || !nb.llama.Running() {
		t.Fatalf("the new embedder did not load (%d ms, %v)", ms, err)
	}
	rel()
	next.Close()
	if na.llama.Running() || nb.llama.Running() {
		t.Fatal("closing the last engine left model servers running")
	}
	if n := len(res.procs); n != 0 {
		t.Fatalf("%d shared process objects left after every engine closed", n)
	}
}

// A model the old engine loads after the new one was built (a request
// still finishing on the old plan) passes to the new engine when it asks
// for it, and is not stopped when the old engine retires.
func TestAModelLoadedLateByTheOldEnginePassesToTheNewOne(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := NewServerResidency(nil)
	dir := t.TempDir()
	lock := twoModelLock(t, modelFile(t, dir, "a.gguf"), modelFile(t, dir, "b.gguf"))
	old := sharedEngine(t, st, res, lock)
	next := sharedEngine(t, st, res, lock)
	t.Cleanup(next.Close)
	oa, na := old.units[keyOf(old, "a")], next.units[keyOf(next, "a")]
	ctx := context.Background()
	rel, ms, err := old.res.Acquire(ctx, oa)
	if err != nil || ms == 0 {
		t.Fatalf("old load: %d ms %v", ms, err)
	}
	rel()
	rel, ms, err = next.res.Acquire(ctx, na)
	if err != nil || ms != 0 {
		t.Fatalf("the new engine loaded a model already running (%d ms, %v)", ms, err)
	}
	rel()
	// The old engine asking again does not take it back.
	rel, _, err = old.res.Acquire(ctx, oa)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	old.Retire(nil)
	if !na.llama.Running() {
		t.Fatal("the old engine stopped a model the new one had taken")
	}
}

// A request on the old engine that holds a changed model keeps it until
// it finishes; only then is it stopped.
func TestAChangedModelInUseStopsWhenItsRequestEnds(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := NewServerResidency(nil)
	dir := t.TempDir()
	brain := modelFile(t, dir, "a.gguf")
	old := sharedEngine(t, st, res, twoModelLock(t, brain, modelFile(t, dir, "b1.gguf")))
	ob := old.units[keyOf(old, "b")]
	if !old.Enter() {
		t.Fatal("no hold")
	}
	relB, _, err := old.res.Acquire(context.Background(), ob)
	if err != nil {
		t.Fatal(err)
	}
	next := sharedEngine(t, st, res, twoModelLock(t, brain, modelFile(t, dir, "b2.gguf")))
	t.Cleanup(next.Close)
	retired := make(chan struct{})
	go func() { old.Retire(nil); close(retired) }()
	time.Sleep(200 * time.Millisecond)
	if !ob.llama.Running() {
		t.Fatal("the changed model was stopped under the request using it")
	}
	relB()
	old.Leave()
	<-retired
	if ob.llama.Running() {
		t.Fatal("the changed model still runs after its request and engine ended")
	}
}

// Engines whose worker starts the same way share one worker; closing the
// older one does not stop it, and the last one to close does.
func TestEnginesShareAnUnchangedWorker(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := NewServerResidency(nil)
	lock := lockWith()
	lock.Python, lock.WorkerScript = filepath.Join(t.TempDir(), "python3"), filepath.Join(t.TempDir(), "brainiac_worker.py")
	a := sharedEngine(t, st, res, lock)
	b := sharedEngine(t, st, res, lock)
	if a.wk == nil || a.wk != b.wk {
		t.Fatal("an unchanged worker was not shared")
	}
	a.Close()
	if h := res.procs; len(h) != 1 {
		t.Fatalf("closing the older engine dropped the worker the newer one uses: %d", len(h))
	}
	changed := *lock
	changed.WorkerScript = filepath.Join(t.TempDir(), "other_worker.py")
	c := sharedEngine(t, st, res, &changed)
	if c.wk == b.wk {
		t.Fatal("a changed worker reused the old one")
	}
	b.Close()
	c.Close()
	if len(res.procs) != 0 {
		t.Fatalf("%d shared process objects left", len(res.procs))
	}
}

// keyOf is the unit key of model m in e.
func keyOf(e *Engine, m string) string {
	for k, u := range e.units {
		if u.model == m {
			return k
		}
	}
	return ""
}
