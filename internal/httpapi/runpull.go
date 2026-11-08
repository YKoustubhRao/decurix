package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// A run's bundle (its recording, ledger, notes, soul trace and manifest) leaves the desktop soon after the run ends
// (plan R9): the final or error callback enqueues a pull, a delete pulls whatever is still missing first, and a
// restart catches up on the last week's runs. The pull itself is runs.Puller's; this file is when it happens.

// RunPuller is what the server needs of runs.Puller.
type RunPuller interface {
	Pull(ctx context.Context, ref runs.ExecutionRef) (runs.Bundle, error)
	FlushDesktop(ctx context.Context, worker string, refs []runs.ExecutionRef, timeout time.Duration) (notPulled []string)
}

// RunBundles says whether an execution's bundle is stored (runs.Store).
type RunBundles interface {
	Has(execID string) (bool, error)
}

// How pulls are queued and tried: a bounded queue (a full one drops, and the delete flush or the next start catches
// what was dropped), a short wait so the desktop's last writes land, and a few tries with a growing wait.
const (
	runPullQueue   = 128
	runPullTries   = 3
	runFlushWithin = 90 * time.Second
	runCatchUp     = 7 * 24 * time.Hour
)

// runPullTimes are the waits of the pull worker: before the first try, and between tries (doubled each time).
// Variables, so tests need not wait.
var runPullDelay, runPullBackoff = 5 * time.Second, 10 * time.Second

// runPulls is the server's pull queue.
type runPulls struct {
	once sync.Once
	ch   chan runs.ExecutionRef
}

// queue is the pull queue, made on first use.
func (p *runPulls) queue() chan runs.ExecutionRef {
	p.once.Do(func() { p.ch = make(chan runs.ExecutionRef, runPullQueue) })
	return p.ch
}

// enqueueRunPull asks for ref's bundle to be pulled. It never blocks: with the queue full the pull is dropped, and
// said (the delete flush and the next start's catch-up pull it then).
func (s *Server) enqueueRunPull(ref runs.ExecutionRef) {
	if s.Runs == nil || ref.ExecID == "" {
		return
	}
	select {
	case s.runPulls.queue() <- ref:
	default:
		log.Printf("runs: the pull queue is full: %s of %s is pulled at its desktop's delete or the next start", ref.ExecID, ref.TaskID)
	}
}

// RunPulls pulls queued bundles, one at a time, until ctx ends. main starts it once.
func (s *Server) RunPulls(ctx context.Context) {
	ch := s.runPulls.queue()
	for {
		select {
		case <-ctx.Done():
			return
		case ref := <-ch:
			s.pullWithRetries(ctx, ref)
		}
	}
}

// pullWithRetries pulls one bundle after runPullDelay, trying up to runPullTries times.
func (s *Server) pullWithRetries(ctx context.Context, ref runs.ExecutionRef) {
	wait := runPullDelay
	for try := 1; try <= runPullTries; try++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		_, err := s.Runs.Pull(ctx, ref)
		if err == nil {
			return
		}
		if errors.Is(err, runs.ErrBadTaskID) {
			log.Printf("runs: %s has no task id a bundle can be read for: not pulled", ref.ExecID)
			return
		}
		log.Printf("runs: pulling %s of %s from %s (try %d of %d): %v", ref.ExecID, ref.TaskID, ref.Worker, try, runPullTries, err)
		wait = runPullBackoff << (try - 1)
	}
}

// unpulledRuns is every ended execution of worker ("" every worker) since since whose bundle is not stored, and,
// with open, the worker's open ones too (a delete pulls what an open run has so far).
func (s *Server) unpulledRuns(worker string, since time.Time, open bool) []runs.ExecutionRef {
	if s.RunBundles == nil {
		return nil
	}
	ended, err := s.Tasks.EndedExecutions(worker, since)
	if err != nil {
		log.Printf("runs: listing ended runs: %v", err)
		return nil
	}
	if open {
		live, err := s.Tasks.OpenExecutions()
		if err != nil {
			log.Printf("runs: listing open runs: %v", err)
		}
		for _, e := range live {
			if e.WorkerID == worker {
				ended = append(ended, e)
			}
		}
	}
	var out []runs.ExecutionRef
	for _, e := range ended {
		if has, err := s.RunBundles.Has(e.ExecID); err == nil && !has {
			out = append(out, runs.ExecutionRef{ExecID: e.ExecID, TaskID: e.TaskID, Worker: e.WorkerID})
		}
	}
	return out
}

// flushRuns pulls every run of a desktop about to be deleted that has no bundle yet (an open one too), within
// runFlushWithin, and returns the runs it could not pull. The delete is refused while any remain, unless a person
// chose to delete anyway (deleteDesktopHandler): a run's labels are never lost silently.
func (s *Server) flushRuns(ctx context.Context, worker string) []runs.ExecutionRef {
	if s.Runs == nil {
		return nil
	}
	refs := s.unpulledRuns(worker, time.Time{}, true)
	if len(refs) == 0 {
		return nil
	}
	byID := make(map[string]runs.ExecutionRef, len(refs))
	for _, ref := range refs {
		byID[ref.ExecID] = ref
	}
	var left []runs.ExecutionRef
	for _, id := range s.Runs.FlushDesktop(ctx, worker, refs, runFlushWithin) {
		ref, ok := byID[id]
		if !ok {
			ref = runs.ExecutionRef{ExecID: id, Worker: worker}
		}
		left = append(left, ref)
	}
	return left
}

// unsavedRunsError refuses a desktop's delete while runs of it are not in the run record yet (the flush did not get
// them in time). Its text is the person's to read; the dashboard offers "Delete anyway" on it (force).
type unsavedRunsError struct{ n int }

// Error says how many runs are not saved and what the person can do.
func (e *unsavedRunsError) Error() string {
	if e.n == 1 {
		return "1 run of this desktop is not saved to the run record yet; try again in a minute, or delete anyway."
	}
	return fmt.Sprintf("%d runs of this desktop are not saved to the run record yet; try again in a minute, or delete anyway.", e.n)
}

// errForceNeedsPerson refuses a "delete anyway" with nobody to author it: giving up runs is a person's choice.
var errForceNeedsPerson = errors.New("only a person can choose to delete a desktop whose runs are not saved")

// recordDeleteAnyway keeps a person's choice to delete worker although the runs left are not saved (rule 5): who,
// through which door, and which runs were given up. The delete goes on only once this is recorded.
func (s *Server) recordDeleteAnyway(worker string, actor tasks.Actor, door string, left []runs.ExecutionRef) (string, error) {
	type givenUp struct {
		ExecutionID string `json:"execution_id"`
		TaskID      string `json:"task_id"`
	}
	list := make([]givenUp, 0, len(left))
	for _, ref := range left {
		list = append(list, givenUp{ExecutionID: ref.ExecID, TaskID: ref.TaskID})
	}
	payload, err := json.Marshal(map[string]any{"runs": list, "flush_seconds": int(runFlushWithin / time.Second)})
	if err != nil {
		return "", err
	}
	return s.Tasks.AddJudgement(tasks.Judgement{
		Actor: actor, Door: door, Kind: tasks.JudgementDeleteAnyway, WorkerID: worker,
		Text:    fmt.Sprintf("Deleted %s with %d runs not saved to the run record", worker, len(left)),
		Payload: payload,
	})
}

// CatchUpRunPulls enqueues the runs of the last week that ended with no bundle, on desktops that still exist: what
// a restart, a full queue or a failed pull left behind. main calls it once at start.
func (s *Server) CatchUpRunPulls(ctx context.Context) {
	if s.Runs == nil {
		return
	}
	exists := map[string]bool{}
	for _, ref := range s.unpulledRuns("", time.Now().Add(-runCatchUp), false) {
		there, seen := exists[ref.Worker]
		if !seen {
			_, err := s.Desktops.Get(ctx, ref.Worker)
			there = err == nil
			exists[ref.Worker] = there
		}
		if there {
			s.enqueueRunPull(ref)
		}
	}
}

// pullAfterOutcome enqueues the bundle pull of an execution that has just ended (a final or an error).
func (s *Server) pullAfterOutcome(kind, worker, taskID, execID string) {
	if execID == "" || (kind != tasks.OutcomeDone && kind != tasks.OutcomeError) {
		return
	}
	s.enqueueRunPull(runs.ExecutionRef{ExecID: execID, TaskID: taskID, Worker: worker})
}
