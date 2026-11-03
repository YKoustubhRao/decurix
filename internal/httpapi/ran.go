package httpapi

import (
	"log"
	"net/http"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// operatorPurpose is the purpose of the operator's own turns: the deployment that served most of them is the one
// that did the work (plan R7).
const operatorPurpose = "operator.turn"

// resolveModel records on an ended execution the deployment that served it: the one with the most operator turns
// in the call log for its task since it started, else the most frequent one, else (a local brain, which the proxy
// never sees) "<engine>:<model>". A failure is logged: the outcome is recorded either way.
func (s *Server) resolveModel(execID string) {
	var lookup tasks.DeploymentLookup
	if s.LLMRec != nil {
		lookup = func(taskID, worker string, since time.Time) (string, error) {
			return s.LLMRec.DeploymentFor(taskID, worker, since, operatorPurpose)
		}
	}
	if _, err := s.Tasks.ResolveModel(execID, lookup); err != nil {
		log.Printf("resolving the model of %s: %v", execID, err)
	}
}

// passRatesHandler is the pass rate per taught skill version and model (GET /api/tasks/pass-rates): runs, checked
// done, a person's correct and wrong, and checked done yet marked wrong. Primary listener, humansOnly.
func (s *Server) passRatesHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	rates, err := s.Tasks.PassRates()
	if err != nil {
		internalError(w, "pass rates", err)
		return
	}
	jsonResponse(w, map[string]any{"pass_rates": rates})
}
