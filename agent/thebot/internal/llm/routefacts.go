package llm

// routefacts.go: what soul learns about a route to a model while running - a request field the endpoint refuses -
// kept under the whole route (provider, model, endpoint). Found in review (2026-11-12, after Hermes
// agent/route_verdicts.py): a refused prompt_cache_key was one process-wide flag, so a deployment that refused it
// switched the key off for every later brain and endpoint the worker was given, and their cache hit rate with it.

import "sync"

// The facts a route can be found to have.
const (
	// factNoCacheKey: the endpoint refused prompt_cache_key; the request is sent without it.
	factNoCacheKey = "no prompt_cache_key"
)

// maxRouteFacts bounds the facts kept: a worker meets a handful of routes, never thousands.
const maxRouteFacts = 256

type routeFact struct {
	route RouteKey
	fact  string
}

// routeFacts is the set of facts learned about routes.
type routeFacts struct {
	mu  sync.Mutex
	set map[routeFact]bool
}

// learnedRoutes is this process's routeFacts.
var learnedRoutes = &routeFacts{}

// has reports whether fact was learned about route.
func (r *routeFacts) has(route RouteKey, fact string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set[routeFact{route, fact}]
}

// learn records fact about route. Past maxRouteFacts the oldest knowledge is dropped all at once: a forgotten fact is
// learned again by one refused call.
func (r *routeFacts) learn(route RouteKey, fact string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set == nil || len(r.set) >= maxRouteFacts {
		r.set = map[routeFact]bool{}
	}
	r.set[routeFact{route, fact}] = true
}

// forget drops every fact (for tests).
func (r *routeFacts) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set = nil
}
