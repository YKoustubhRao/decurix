// runsearch_routes.go serves the search over past runs (internal/runsearch) to a person and, scoped to itself, to a
// desktop.

package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/runsearch"
)

// Searching past runs (internal/runsearch): the stored text of every task, found by words, read in order, or
// scrolled around one text. Two doors, both on the primary listener only:
//
//   - GET /api/runs/search is a person's (humansOnly): any worker's runs, or one worker's with ?worker=. The Tasks
//     page's search box calls it.
//   - GET /api/agent/runs is a desktop's callback (containerRoutes, read only): the calling desktop's own runs and
//     nothing else, whatever the request names (actingAs). The agent applies the client scope of its memory to what
//     it is given (agent/decurix_agent/run_search.py).
//
// The shape is told by the query: ?q=WORDS is a discovery, ?task=ID a read, ?task=ID&around=SEQ a scroll. Every shape
// takes the stored text, cut at runsearch.MaxMessageChars a text and said so; nothing is summarised.

// runSearchPersonHandler answers GET /api/runs/search for a person.
func (s *Server) runSearchPersonHandler(w http.ResponseWriter, r *http.Request) {
	s.runSearch(w, r, strings.TrimSpace(r.URL.Query().Get("worker")))
}

// runSearchDesktopHandler answers GET /api/agent/runs: the calling desktop's own runs only. From the host it serves
// the desktop the query names, and needs one.
func (s *Server) runSearchDesktopHandler(w http.ResponseWriter, r *http.Request) {
	worker, ok := actingAs(w, r, r.URL.Query().Get("desktop_id"))
	if !ok {
		return
	}
	if strings.TrimSpace(worker) == "" {
		jsonStatusError(w, http.StatusBadRequest, "desktop_id is required")
		return
	}
	s.runSearch(w, r, worker)
}

// runSearch serves one search shape, held to worker's runs when worker is set.
func (s *Server) runSearch(w http.ResponseWriter, r *http.Request, worker string) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.RunSearch == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "past runs are not searchable here")
		return
	}
	q := r.URL.Query()
	task := strings.TrimSpace(q.Get("task"))
	if task != "" {
		if !execIDPattern.MatchString(task) {
			jsonStatusError(w, http.StatusBadRequest, "task is not a task id")
			return
		}
		var run runsearch.Run
		var err error
		shape := "read"
		if around := strings.TrimSpace(q.Get("around")); around != "" {
			seq, aerr := strconv.Atoi(around)
			if aerr != nil || seq < 0 {
				jsonStatusError(w, http.StatusBadRequest, "around is the number of a text of the run")
				return
			}
			shape = "scroll"
			run, err = s.RunSearch.Scroll(task, worker, seq, atoiOr(q.Get("window"), 0))
		} else {
			run, err = s.RunSearch.Read(task, worker, atoiOr(q.Get("limit"), 0))
		}
		if errors.Is(err, runsearch.ErrNotFound) {
			jsonStatusError(w, http.StatusNotFound, "no such run")
			return
		}
		if err != nil {
			internalError(w, "run search "+shape, err)
			return
		}
		jsonResponse(w, map[string]any{"shape": shape, "run": run})
		return
	}

	now := time.Now().UTC()
	after, err := runsearch.ParseBound(q.Get("after"), now)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "after: a date (2026-07-01), a time (RFC 3339) or a span back from now (24h, 7d, 2w)")
		return
	}
	before, err := runsearch.ParseBound(q.Get("before"), now)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "before: a date (2026-07-01), a time (RFC 3339) or a span back from now (24h, 7d, 2w)")
		return
	}
	var exclude []string
	for _, id := range strings.Split(q.Get("exclude"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			exclude = append(exclude, id)
		}
	}
	res, err := s.RunSearch.Search(runsearch.Query{Words: q.Get("q"), Worker: worker, After: after, Before: before,
		Exclude: exclude, Limit: atoiOr(q.Get("limit"), 0)})
	if errors.Is(err, runsearch.ErrNoWords) {
		jsonStatusError(w, http.StatusBadRequest, "give words to search for (q), or a task to read")
		return
	}
	if err != nil {
		internalError(w, "run search", err)
		return
	}
	jsonResponse(w, map[string]any{"shape": "discover", "hits": res.Hits, "tasks": res.Tasks, "note": res.Note})
}

// atoiOr is s as a number, or def when it is not one.
func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}
