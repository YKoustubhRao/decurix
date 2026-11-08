package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNoRoom is a model that cannot be loaded without taking memory other work
// is using. Brainiac refuses rather than push the machine into swap.
type ErrNoRoom struct {
	Model    string
	NeedGB   float64
	FreeGB   float64
	Pool     string
	Unloaded []string
}

// Error says what was needed and what there was.
func (e *ErrNoRoom) Error() string {
	return fmt.Sprintf("%s needs %.1f GB but the %s pool has %.1f GB free even after unloading Brainiac's own models; close something or use a remote brain",
		e.Model, e.NeedGB, e.Pool, e.FreeGB)
}

// loadable is what the residency manager moves in and out of memory.
type loadable interface {
	key() string
	pool() string
	memGB() float64
	load(ctx context.Context) error
	unload()
	// alive reports whether what was loaded is still there: a model server
	// can die after loading (an OOM kill), and then it must load again.
	alive() bool
}

type resident struct {
	u        loadable
	refs     int
	lastUsed time.Time
}

// recipeOf is what started u's process, when u says (a unit does): two
// loadables with the same key and recipe run the same process, so an engine
// built later uses the one already in memory. "" is a loadable that does not
// say, known by its key alone.
func recipeOf(u loadable) string {
	if r, ok := u.(interface{ recipe() string }); ok {
		return r.recipe()
	}
	return ""
}

// generationOf is the engine generation that built u (0 when it does not say).
func generationOf(u loadable) uint64 {
	if g, ok := u.(interface{ generation() uint64 }); ok {
		return g.generation()
	}
	return 0
}

// ident is the residency's name for u: its key, and its recipe when it has
// one. A model whose recipe changed (a new file, context or build) is
// another resident, never handed the old process.
func ident(u loadable) string {
	if r := recipeOf(u); r != "" {
		return u.key() + "#" + r
	}
	return u.key()
}

// sharedHandle is one process object several engines may use in turn (a
// model server, the Python worker), by its recipe; it is stopped when the
// last engine using it lets go.
type sharedHandle struct {
	v     any
	users int
	stop  func()
}

// Residency decides which models are in memory. Each pool has a budget (from
// the plan); a model that does not fit beside the others evicts the least
// recently used ones Brainiac itself loaded and is not using right now.
// Before loading, the pool's real free memory is measured again, because the
// machine is shared: desktops come and go.
//
// One Residency belongs to a server and outlives its engines (G-11): an
// engine built for a new plan uses the processes of the units whose recipe
// did not change (adopt), and the engine it replaced stops only its own
// (the ones that changed or left the plan) when it retires.
type Residency struct {
	mu      sync.Mutex
	budgets map[string]float64
	loaded  map[string]*resident // by ident
	// procs are the process objects engines share, by recipe.
	procs map[string]*sharedHandle
	gen   uint64
	// live re-measures a pool's available memory; nil skips the check.
	live func(ctx context.Context, pool string) (float64, error)
	// loadMu serialises loads per pool: two big loads at once on a small
	// machine is how it ends up swapping.
	loadMu map[string]*sync.Mutex
}

// NewResidency returns a manager with per-pool budgets in GB.
func NewResidency(budgets map[string]float64, live func(ctx context.Context, pool string) (float64, error)) *Residency {
	return &Residency{budgets: budgets, loaded: map[string]*resident{}, live: live, loadMu: map[string]*sync.Mutex{},
		procs: map[string]*sharedHandle{}}
}

// NewServerResidency is the residency a server keeps for all its engines:
// budgets come from each engine's plan, and the pool's free memory is
// measured on this machine before each load.
func NewServerResidency(live func(ctx context.Context, pool string) (float64, error)) *Residency {
	return NewResidency(map[string]float64{}, live)
}

// setBudgets replaces the per-pool budgets (a new plan's).
func (r *Residency) setBudgets(b map[string]float64) {
	r.mu.Lock()
	r.budgets = b
	r.mu.Unlock()
}

// nextGeneration numbers a new engine; a later engine's units take over
// residents from an earlier one's.
func (r *Residency) nextGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	return r.gen
}

// share returns the process object running recipe if an engine holds one,
// else v, which is registered under it; stop ends v's process when the last
// engine releases it. Each share is paired with one release.
func (r *Residency) share(recipe string, v any, stop func()) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.procs[recipe]; ok {
		h.users++
		return h.v
	}
	r.procs[recipe] = &sharedHandle{v: v, users: 1, stop: stop}
	return v
}

// release lets go of shared process objects; one no engine uses any more
// is stopped.
func (r *Residency) release(recipes []string) {
	var stops []func()
	r.mu.Lock()
	for _, k := range recipes {
		h, ok := r.procs[k]
		if !ok {
			continue
		}
		if h.users--; h.users <= 0 {
			delete(r.procs, k)
			if h.stop != nil {
				stops = append(stops, h.stop)
			}
		}
	}
	r.mu.Unlock()
	for _, s := range stops {
		s()
	}
}

// adopt hands the residents whose ident matches one of units to that unit:
// the new engine owns them from now on, so the engine it replaces does not
// stop them when it retires. Holds already taken stay valid. It returns the
// keys it kept.
func (r *Residency) adopt(units []loadable) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var kept []string
	for _, u := range units {
		if e, ok := r.loaded[ident(u)]; ok && generationOf(u) >= generationOf(e.u) {
			inheritFrom(u, e.u)
			e.u = u
			kept = append(kept, u.key())
		}
	}
	sort.Strings(kept)
	return kept
}

// Acquire makes u resident and holds it until release is called. loadMs is
// the time spent loading (0 when it was already in memory).
func (r *Residency) Acquire(ctx context.Context, u loadable) (release func(), loadMs int64, err error) {
	// A model already in memory needs no load lock: it never waits behind
	// another model's load in the same pool.
	id := ident(u)
	r.mu.Lock()
	if e, ok := r.loaded[id]; ok && e.u.alive() {
		e.refs++
		e.lastUsed = time.Now()
		r.takeLocked(e, u)
		r.mu.Unlock()
		return r.releaser(id, e), 0, nil
	}
	lm := r.loadMu[u.pool()]
	if lm == nil {
		lm = &sync.Mutex{}
		r.loadMu[u.pool()] = lm
	}
	r.mu.Unlock()

	lm.Lock()
	defer lm.Unlock()

	r.mu.Lock()
	if e, ok := r.loaded[id]; ok {
		if e.u.alive() {
			e.refs++
			e.lastUsed = time.Now()
			r.takeLocked(e, u)
			r.mu.Unlock()
			return r.releaser(id, e), 0, nil
		}
		// It died after loading: its memory is back already. Forget it, and
		// load it again below. Requests still holding the dead one release
		// their own entry, not the new one.
		r.forgetLocked(id)
	}
	evicted, err := r.makeRoomLocked(ctx, u)
	r.mu.Unlock()
	for _, e := range evicted {
		e.unload()
	}
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	if err := u.load(ctx); err != nil {
		return nil, 0, err
	}
	e := &resident{u: u, refs: 1, lastUsed: time.Now()}
	r.mu.Lock()
	r.loaded[id] = e
	r.mu.Unlock()
	return r.releaser(id, e), time.Since(start).Milliseconds(), nil
}

// takeLocked gives e to u when u's engine is newer than its owner's: a
// model an old engine loaded after the new one was built still passes to
// the new one, and never back.
func (r *Residency) takeLocked(e *resident, u loadable) {
	if generationOf(u) > generationOf(e.u) {
		inheritFrom(u, e.u)
		e.u = u
	}
}

// inheritFrom lets u take over what old knew about the running model.
func inheritFrom(u, old loadable) {
	if h, ok := u.(interface{ inherit(loadable) }); ok {
		h.inherit(old)
	}
}

// releaser gives back one hold on e. It touches e only while e is still the
// resident for key: one that was forgotten and loaded again is a new entry.
func (r *Residency) releaser(key string, e *resident) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if cur, ok := r.loaded[key]; ok && cur == e && e.refs > 0 {
				e.refs--
				e.lastUsed = time.Now()
			}
			r.mu.Unlock()
		})
	}
}

// makeRoomLocked picks the residents to evict so u fits, both in the plan's
// budget and in the memory the pool really has free now.
func (r *Residency) makeRoomLocked(ctx context.Context, u loadable) ([]loadable, error) {
	pool := u.pool()
	need := u.memGB()
	var mine []*resident
	used := 0.0
	for _, e := range r.loaded {
		if e.u.pool() == pool {
			used += e.u.memGB()
			mine = append(mine, e)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].lastUsed.Before(mine[j].lastUsed) })

	free := -1.0
	if r.live != nil {
		if f, err := r.live(ctx, pool); err == nil {
			free = f
		}
	}
	budget, hasBudget := r.budgets[pool]
	fits := func(freed float64) bool {
		if hasBudget && used-freed+need > budget+0.01 {
			return false
		}
		// What our own residents free comes back to the pool when they go.
		return free < 0 || free+freed >= need*0.9
	}
	var evict []loadable
	freed := 0.0
	for _, e := range mine {
		if fits(freed) {
			break
		}
		if e.refs > 0 {
			continue
		}
		evict = append(evict, e.u)
		freed += e.u.memGB()
	}
	if !fits(freed) {
		var names []string
		for _, e := range evict {
			names = append(names, e.key())
		}
		f := free
		if f < 0 {
			f = budget - used
		}
		return nil, &ErrNoRoom{Model: u.key(), NeedGB: need, FreeGB: f + freed, Pool: pool, Unloaded: names}
	}
	for _, e := range evict {
		delete(r.loaded, ident(e))
	}
	return evict, nil
}

// Loaded lists the resident models' keys.
func (r *Residency) Loaded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range r.loaded {
		if k := e.u.key(); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Forget drops a resident without unloading it (it already went away).
func (r *Residency) Forget(key string) {
	r.mu.Lock()
	r.forgetLocked(key)
	r.mu.Unlock()
}

func (r *Residency) forgetLocked(key string) { delete(r.loaded, key) }

// UnloadIdle unloads every resident no one holds now; held ones stay. Each
// pool is done under its load lock, the one eviction and loading hold, so a
// request that acquires the same model meanwhile waits for the unload and
// then loads it again, instead of having it stopped under it.
func (r *Residency) UnloadIdle() { r.unloadIdle(nil) }

// unloadIdle is UnloadIdle for the residents owned says are its (nil: all).
func (r *Residency) unloadIdle(owned func(loadable) bool) {
	r.mu.Lock()
	pools := map[string]bool{}
	for _, e := range r.loaded {
		if owned == nil || owned(e.u) {
			pools[e.u.pool()] = true
		}
	}
	r.mu.Unlock()
	names := make([]string, 0, len(pools))
	for p := range pools {
		names = append(names, p)
	}
	sort.Strings(names) // one pool at a time, in a fixed order
	for _, pool := range names {
		r.mu.Lock()
		lm := r.loadMu[pool]
		if lm == nil {
			lm = &sync.Mutex{}
			r.loadMu[pool] = lm
		}
		r.mu.Unlock()
		lm.Lock()
		r.mu.Lock()
		var idle []loadable
		for k, e := range r.loaded {
			if e.refs == 0 && e.u.pool() == pool && (owned == nil || owned(e.u)) {
				idle = append(idle, e.u)
				delete(r.loaded, k)
			}
		}
		r.mu.Unlock()
		for _, u := range idle {
			u.unload()
		}
		lm.Unlock()
	}
}

// UnloadAll unloads every resident (shutdown).
func (r *Residency) UnloadAll() { r.unloadAll(nil) }

// unloadAll is UnloadAll for the residents owned says are its (nil: all).
func (r *Residency) unloadAll(owned func(loadable) bool) {
	r.mu.Lock()
	var all []loadable
	for k, e := range r.loaded {
		if owned == nil || owned(e.u) {
			all = append(all, e.u)
			delete(r.loaded, k)
		}
	}
	r.mu.Unlock()
	for _, u := range all {
		u.unload()
	}
}
