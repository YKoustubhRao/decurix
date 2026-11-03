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

// Residency decides which models are in memory. Each pool has a budget (from
// the plan); a model that does not fit beside the others evicts the least
// recently used ones Brainiac itself loaded and is not using right now.
// Before loading, the pool's real free memory is measured again, because the
// machine is shared: desktops come and go.
type Residency struct {
	mu      sync.Mutex
	budgets map[string]float64
	loaded  map[string]*resident
	// live re-measures a pool's available memory; nil skips the check.
	live func(ctx context.Context, pool string) (float64, error)
	// loadMu serialises loads per pool: two big loads at once on a small
	// machine is how it ends up swapping.
	loadMu map[string]*sync.Mutex
}

// NewResidency returns a manager with per-pool budgets in GB.
func NewResidency(budgets map[string]float64, live func(ctx context.Context, pool string) (float64, error)) *Residency {
	return &Residency{budgets: budgets, loaded: map[string]*resident{}, live: live, loadMu: map[string]*sync.Mutex{}}
}

// Acquire makes u resident and holds it until release is called. loadMs is
// the time spent loading (0 when it was already in memory).
func (r *Residency) Acquire(ctx context.Context, u loadable) (release func(), loadMs int64, err error) {
	r.mu.Lock()
	lm := r.loadMu[u.pool()]
	if lm == nil {
		lm = &sync.Mutex{}
		r.loadMu[u.pool()] = lm
	}
	r.mu.Unlock()

	lm.Lock()
	defer lm.Unlock()

	r.mu.Lock()
	if e, ok := r.loaded[u.key()]; ok {
		if e.u.alive() {
			e.refs++
			e.lastUsed = time.Now()
			r.mu.Unlock()
			return r.releaser(u.key(), e), 0, nil
		}
		// It died after loading: its memory is back already. Forget it, and
		// load it again below. Requests still holding the dead one release
		// their own entry, not the new one.
		r.forgetLocked(u.key())
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
	r.loaded[u.key()] = e
	r.mu.Unlock()
	return r.releaser(u.key(), e), time.Since(start).Milliseconds(), nil
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
		delete(r.loaded, e.key())
	}
	return evict, nil
}

// Loaded lists the resident models' keys.
func (r *Residency) Loaded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k := range r.loaded {
		out = append(out, k)
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

// UnloadAll unloads every resident (shutdown).
func (r *Residency) UnloadAll() {
	r.mu.Lock()
	var all []loadable
	for _, e := range r.loaded {
		all = append(all, e.u)
	}
	r.loaded = map[string]*resident{}
	r.mu.Unlock()
	for _, u := range all {
		u.unload()
	}
}
