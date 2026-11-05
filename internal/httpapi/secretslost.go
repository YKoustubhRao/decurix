package httpapi

import (
	"log"
	"sync"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// What a person marked secret is held in memory only (secretsSeen), so a control-plane restart forgets it while a
// task that was open goes on re-sending its transcript. Until each such task ends, its desktop's model calls are
// recorded with hashes only, no text (llmrec.Call.NoBodies): their secrets are unknown here now (review CP-A R1).

// secretsLost holds, per desktop, the tasks that were open when the control plane started.
type secretsLost struct {
	mu     sync.Mutex
	byDesk map[string]map[string]bool
}

// ForgetSecretsAtStart marks every desktop with a task open now as one whose secrets are unknown: main calls it
// once at start, before any desktop's call is recorded.
func (s *Server) ForgetSecretsAtStart() {
	open, err := s.Tasks.OpenExecutions()
	if err != nil {
		log.Printf("llm calls: listing the tasks open at start: %v; every call is recorded as usual", err)
		return
	}
	s.lostSecrets.mu.Lock()
	defer s.lostSecrets.mu.Unlock()
	s.lostSecrets.byDesk = map[string]map[string]bool{}
	for _, e := range open {
		if s.lostSecrets.byDesk[e.WorkerID] == nil {
			s.lostSecrets.byDesk[e.WorkerID] = map[string]bool{}
		}
		s.lostSecrets.byDesk[e.WorkerID][e.TaskID] = true
	}
	if n := len(s.lostSecrets.byDesk); n > 0 {
		log.Printf("llm calls: %d desktop(s) had a task open at start: their calls keep no text until those tasks end", n)
	}
}

// secretsUnknown reports whether desktop's calls are to be recorded with no text now.
func (s *Server) secretsUnknown(desktop string) bool {
	s.lostSecrets.mu.Lock()
	defer s.lostSecrets.mu.Unlock()
	return len(s.lostSecrets.byDesk[desktop]) > 0
}

// desktopTasksEnded lets a desktop's calls keep their text again once its open tasks were closed (a stop, a delete).
func (s *Server) desktopTasksEnded(desktop string) {
	s.lostSecrets.mu.Lock()
	defer s.lostSecrets.mu.Unlock()
	delete(s.lostSecrets.byDesk, desktop)
}

// taskEnded lets a desktop's calls keep their text again once the last task it had open at start has ended.
func (s *Server) taskEnded(desktop, taskID string) {
	s.lostSecrets.mu.Lock()
	defer s.lostSecrets.mu.Unlock()
	if tasks := s.lostSecrets.byDesk[desktop]; tasks != nil {
		delete(tasks, taskID)
		if len(tasks) == 0 {
			delete(s.lostSecrets.byDesk, desktop)
		}
	}
}

// heldFor is what the records withhold of desktop worker's own values: what its person marked secret (secretsSeen)
// and its screen password, which its worker can read (desktop.ScreenHeld, review RES-CP-1 B7).
func (s *Server) heldFor(worker string) []string {
	return append(s.secretsSeen.For(worker), desktop.ScreenHeld(worker)...)
}
