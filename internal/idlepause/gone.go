package idlepause

import (
	"context"
	"log"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Closing the tasks of desktops that no longer exist (C-17). An execution was closed only by its agent's report or by
// a stop, restart or delete through the API. A desktop whose container was removed or crashed while the control plane
// was down - or crashed without its agent reporting - kept its execution "running" for good: Working() then kept the
// PC awake, and Busy() refused the worker anything new. Such an execution is now ended as "desktop gone", at start-up
// and on every idle sweep, but only when Docker says the container is not there: an error asking is no evidence.

// goneWhy is what a task closed this way is told.
const goneWhy = "The desktop this task ran on no longer exists (its container is gone); the task ended there."

// DesktopGone is what closing gone desktops' tasks needs from desktop.Manager: whether a desktop's container is gone.
// Satisfied by *desktop.Manager.
type DesktopGone interface {
	Gone(ctx context.Context, id string) (bool, error)
}

// GoneTasks is what it needs from tasks.Store: the workers with a live execution, a worker's record (to tell a desktop
// from the control plane's own workers), and CloseOpen.
type GoneTasks interface {
	LiveWorkers() ([]string, error)
	Worker(id string) (tasks.Worker, bool)
	CloseOpen(workerID, why string) error
}

// ownWorkerProfiles are the profiles of workers the control plane runs itself, outside any desktop container: a
// practice desktop, whatever runs in it (Simulate's "practice" engine, an eval's "eval"), and a readiness run (no
// container at all). Their live executions are their runner's to close; a running one must never be ended as "gone" by
// an idle sweep. Matched by profile, the kind of desktop, not by engine name: an eval case's execution was closed as
// abandoned within about two minutes, cutting its run record short, because "eval" was not one of the engines named
// (FALSE-DONE item D).
var ownWorkerProfiles = map[string]bool{"practice": true, "offline-artifacts": true}

// CloseGoneAtStart ends the live tasks of every worker whose desktop container is gone, practice and readiness
// workers included: at start-up nothing of theirs is still running (main cleans up their containers first). store is
// the tasks store; a store without LiveWorkers leaves this undone, and says so.
func CloseGoneAtStart(ctx context.Context, desktops DesktopGone, store any) {
	closeGone(ctx, desktops, store, true)
}

// closeGone is CloseGoneAtStart and the idle sweep's pass; all says whether the control plane's own workers count.
func closeGone(ctx context.Context, desktops DesktopGone, store any, all bool) {
	t, ok := store.(GoneTasks)
	if !ok || desktops == nil {
		if all {
			log.Printf("idlepause: the tasks store cannot list workers with live tasks; tasks of desktops that are gone stay open")
		}
		return
	}
	live, err := t.LiveWorkers()
	if err != nil {
		log.Printf("idlepause: listing workers with live tasks: %v", err)
		return
	}
	for _, id := range live {
		if w, known := t.Worker(id); known && !all && ownWorkerProfiles[w.Profile] {
			continue
		}
		gone, err := desktops.Gone(ctx, id)
		if err != nil || !gone {
			continue
		}
		if err := t.CloseOpen(id, goneWhy); err != nil {
			log.Printf("idlepause: closing the tasks of gone desktop %s: %v", id, err)
			continue
		}
		log.Printf("idlepause: desktop %s no longer exists; its open tasks are closed", id)
	}
}
