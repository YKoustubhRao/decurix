package dag

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YKoustubhRao/decurix/internal/schedule"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Submitter dispatches a (placeholder-substituted) node's request to a
// desktop and hands back the task ID it landed under, so a run can watch
// that specific task to completion. Satisfied by
// *httpapi.Server.SubmitTracked; narrowed to an interface so this package
// needs no import of httpapi, the same inversion internal/schedule's own
// Submitter already uses. Named SubmitTracked, not Submit: *httpapi.Server
// already has its own Submit (internal/schedule's Submitter shape, which
// returns no task ID), and Go match interfaces by method name - reusing
// "Submit" here would collide with that existing, differently-shaped method.
type Submitter interface {
	SubmitTracked(ctx context.Context, desktopID, content string, origin tasks.Origin) (taskID string, err error)
}

// TaskResults is what this package needs from internal/tasks.Store: a
// dispatched node's current status and, once terminal, the reply it ended
// with - what a later node's output placeholder pulls from - plus whether a
// desktop is currently busy, checked immediately before a node dispatches to
// it. Satisfied by *tasks.Store.
type TaskResults interface {
	// TaskResult also says whether the task's final check judged its result (nil: the desktop did not say): only a
	// checked "done" lets a node succeed and feed its reply to the next one.
	TaskResult(taskID string) (status string, checked *bool, finalReply string, ok bool)

	// Busy reports why a desktop cannot take on a new ticket right now, or
	// "" when it can - see tasks.Store.Busy. dispatch checks this itself
	// rather than relying on internal/tasks's own queueing: that queueing
	// (and absorbQueuedReply) exists for one human typing one message at a
	// time, and can silently delete a second automated ticket's record if
	// the first one it landed behind then pauses for a query or approval.
	Busy(desktopID string) (string, error)
}

// defaultInterval is how often Run checks for due cron DAGs and advances
// active runs, when Interval is unset.
const defaultInterval = 20 * time.Second

// dispatchConcurrency bounds how many cron DAGs are fired, how many active
// runs are advanced, and how many of one run's ready nodes are dispatched,
// at once - mirrors internal/schedule.fireConcurrency /
// internal/idlepause.sweepConcurrency, but nests at two levels here (runs,
// then that run's ready nodes), so a single tick can have up to
// dispatchConcurrency^2 dispatch() calls in flight across all active runs,
// not just dispatchConcurrency.
const dispatchConcurrency = 8

// claimLease is how long a node may stay claimed with no task ID before the run gives up on it: the claim is made
// before the ticket is sent, and a control plane stopped (or a send stuck) in between leaves it so for good. Longer
// than any desktop takes to wake (minutes), so a send still in flight is not cut off.
const claimLease = 15 * time.Minute

// Why a node failed without its task failing, in words a person can act on.
const (
	reasonUnchecked   = "Finished without a checked final report, so nothing was built on it. The worker said: "
	reasonStuckClaim  = "The control plane stopped (or the send stalled) while sending this node's request, so whether it reached the desktop is unknown: check the desktop before running again."
	reasonUnrecorded  = "The request was sent, but the run could not record its task, so it cannot follow it: check the desktop (task %s)."
	reasonCancelledBy = "Cancelled by a person."
)

// Runner drives every DAG's runs forward: fires a due cron trigger, and
// advances each active run by resolving any node whose ticket has finished
// and dispatching whatever is now ready.
//
// A manual trigger (Trigger, called synchronously from
// internal/httpapi's POST /api/dags/run) can race this same advance() over
// the very run it just started, if a tick happens to land in that window -
// unlike internal/schedule/internal/idlepause, whose loops never have a
// synchronous external caller touching the row they are sweeping. Every
// run-node/run status transition is therefore a compare-and-swap
// (dag.go/run.go's ClaimRunNode/MarkRunNodeDispatched/MarkRunNodeTerminal/
// SkipRunNode/FinishRun all report whether they actually applied): the loser
// of a race no-ops instead of double-dispatching a node or double-closing a
// run. A node is claimed (ClaimRunNode) before its ticket is ever submitted,
// specifically so the loser of that race never calls Submitter at all - see
// dispatch.
type Runner struct {
	Store     *Store
	Submitter Submitter
	Tasks     TaskResults
	Interval  time.Duration // defaultInterval if zero

	// now is the test seam - defaults to time.Now().UTC.
	now func() time.Time

	// startedNS is when this process's Runner began (UnixNano; 0 until Run): a claim older than that can have no send
	// in flight here, so it is given up at once, without waiting out claimLease.
	startedNS atomic.Int64

	// desktopLocksMu guards desktopLocks, lazily populated with one mutex
	// per desktop ID ever dispatched to. dispatch holds a desktop's mutex
	// across its Busy check and the Submitter call, so two run-nodes ready
	// for the same desktop in the same tick - the same run (a fan-out that
	// rejoins on one desktop) or two different runs - can't both see it
	// free and both submit; see TaskResults.Busy's doc for why that would
	// be unsafe. Entries are never removed - one mutex per desktop ID for
	// the process lifetime, a bounded and small cost.
	desktopLocksMu sync.Mutex
	desktopLocks   map[string]*sync.Mutex
}

// desktopDispatchLock returns the mutex serializing dispatch decisions for
// one desktop.
func (r *Runner) desktopDispatchLock(desktopID string) *sync.Mutex {
	r.desktopLocksMu.Lock()
	defer r.desktopLocksMu.Unlock()

	if r.desktopLocks == nil {
		r.desktopLocks = make(map[string]*sync.Mutex)
	}

	mu, ok := r.desktopLocks[desktopID]
	if !ok {
		mu = &sync.Mutex{}
		r.desktopLocks[desktopID] = mu
	}

	return mu
}

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}

	return time.Now().UTC()
}

// Run checks for due cron DAGs and advances active runs every Interval
// until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	r.markStarted(r.clock())

	schedule.Every(ctx, interval, r.tick)
}

func (r *Runner) tick(ctx context.Context) {
	now := r.clock()

	due, err := r.Store.DueCron(now)
	if err != nil {
		log.Printf("dag: checking due cron dags: %v", err)
	} else {
		schedule.Concurrently(due, dispatchConcurrency, func(d DAG) { r.fireCron(ctx, d, now) })
	}

	active, err := r.Store.ActiveRuns()
	if err != nil {
		log.Printf("dag: checking active runs: %v", err)
		return
	}

	schedule.Concurrently(active, dispatchConcurrency, func(run Run) { r.advance(ctx, run) })
}

// fireCron starts a run for a due DAG and advances its cron to the next
// occurrence regardless of whether the run itself could be started (the DAG
// might be headless, say) - a temporary failure must not turn every
// following tick into a retry of the same stale occurrence, mirroring
// internal/schedule.Runner.fire. Unlike that Runner, no "TIME:{...}" field
// is appended to any node's content: that marker is specific to a
// schedule's resend-the-identical-text model, and a DAG node's content is
// user-authored (and may itself carry output placeholders).
func (r *Runner) fireCron(ctx context.Context, d DAG, now time.Time) {
	if _, err := r.Trigger(ctx, d.ID, TriggerCron); err != nil {
		log.Printf("dag: firing %s: %v", d.ID, err)
	}

	if err := r.Store.MarkCronRun(d.ID, now); err != nil {
		log.Printf("dag: advancing %s: %v", d.ID, err)
	}
}

// Trigger starts a new run of dagID and immediately advances it once, so a
// manual "run now" (and a cron firing) dispatch the head without waiting
// for the next tick - the same immediacy internal/httpapi's
// sendRequestHandler already gives a one-off send.
func (r *Runner) Trigger(ctx context.Context, dagID, source string) (Run, error) {
	run, err := r.Store.StartRun(dagID, source, r.clock())
	if err != nil {
		return Run{}, err
	}

	r.advance(ctx, run)

	if updated, err := r.Store.getRun(run.ID); err == nil {
		return updated, nil
	}

	return run, nil
}

// advance moves run forward by one step: resolve any dispatched node whose
// task has finished (propagating Skip to whatever only that failure could
// reach), dispatch whatever is now ready, and close the run if nothing is
// left to do.
func (r *Runner) advance(ctx context.Context, run Run) {
	nodes, err := r.Store.Nodes(run.DAGID)
	if err != nil {
		log.Printf("dag: advancing run %s: %v", run.ID, err)
		return
	}

	edges, err := r.Store.Edges(run.DAGID)
	if err != nil {
		log.Printf("dag: advancing run %s: %v", run.ID, err)
		return
	}

	nodeByID := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}

	now := r.clock()

	runNodes, err := r.Store.RunNodes(run.ID)
	if err != nil {
		log.Printf("dag: advancing run %s: %v", run.ID, err)
		return
	}

	r.resolveRunning(run.ID, runNodes, edges, now)

	runNodes, err = r.Store.RunNodes(run.ID)
	if err != nil {
		log.Printf("dag: advancing run %s: %v", run.ID, err)
		return
	}

	r.dispatchReady(ctx, run.ID, nodeByID, nodes, edges, runNodes, now)

	runNodes, err = r.Store.RunNodes(run.ID)
	if err != nil {
		log.Printf("dag: advancing run %s: %v", run.ID, err)
		return
	}

	closeIfDone(r.Store, run.ID, runNodes, now)
}

// resolveRunning polls every dispatched (running) run-node's task; once it
// has gone terminal, marks the run-node succeeded (task status done) or
// failed (anything else terminal), propagating Skip downstream on failure.
func (r *Runner) resolveRunning(runID string, runNodes []RunNode, edges []Edge, now time.Time) {
	for _, rn := range runNodes {
		if rn.Status != NodeRunning {
			continue
		}

		if rn.TaskID == "" {
			// Claimed and never given its task (C-02): failed with a reason, never silently sent again - the
			// request may have reached the desktop (the work queue's HoldInterrupted rule).
			if started := r.startedNS.Load(); started != 0 && rn.StartedAt.UnixNano() < started ||
				now.Sub(rn.StartedAt) > claimLease {
				r.fail(runID, rn, reasonStuckClaim, edges, now)
			}
			continue
		}

		status, checked, reply, ok := r.Tasks.TaskResult(rn.TaskID)
		if !ok || tasks.Live(status) {
			continue
		}

		toStatus := NodeFailed
		if outcome := tasks.DisplayOutcome(status, checked); outcome.Done() {
			toStatus = NodeSucceeded
		} else if outcome.Finished() {
			// The worker's word alone: never fed to {{parent.reply}} downstream (C-13).
			reply = reasonUnchecked + reply
		}

		applied, err := r.Store.MarkRunNodeTerminal(rn.ID, NodeRunning, toStatus, reply, now)
		if err != nil {
			log.Printf("dag: resolving node %s in run %s: %v", rn.NodeID, runID, err)
			continue
		}

		if applied && toStatus == NodeFailed {
			r.skipDescendants(runID, rn.NodeID, edges, now)
		}
	}
}

// dispatchReady submits the ticket for every pending run-node whose parents
// have all succeeded, substituting output placeholders into its content
// first.
func (r *Runner) dispatchReady(ctx context.Context, runID string, nodeByID map[string]Node, nodes []Node, edges []Edge, runNodes []RunNode, now time.Time) {
	byNodeID := make(map[string]RunNode, len(runNodes))
	for _, rn := range runNodes {
		byNodeID[rn.NodeID] = rn
	}

	parentsOf := make(map[string][]string)
	for _, e := range edges {
		parentsOf[e.ToNodeID] = append(parentsOf[e.ToNodeID], e.FromNodeID)
	}

	var ready []RunNode

	for _, rn := range runNodes {
		if _, ok := nodeByID[rn.NodeID]; ok && rn.Status == NodePending && parentsSatisfied(parentsOf[rn.NodeID], byNodeID) {
			ready = append(ready, rn)
		}
	}

	schedule.Concurrently(ready, dispatchConcurrency, func(rn RunNode) {
		r.dispatch(ctx, runID, rn, nodeByID[rn.NodeID], nodes, edges, runNodes, now)
	})
}

// parentsSatisfied reports whether every parent that actually has a
// run-node in this run has succeeded. A parent with no run-node here is a
// node outside the set reachable from the head - it will never resolve, so
// waiting on it would hang the join forever; it is simply not counted.
func parentsSatisfied(parentIDs []string, byNodeID map[string]RunNode) bool {
	for _, parentID := range parentIDs {
		parent, inRun := byNodeID[parentID]
		if !inRun {
			continue
		}

		if parent.Status != NodeSucceeded {
			return false
		}
	}

	return true
}

func (r *Runner) dispatch(ctx context.Context, runID string, rn RunNode, node Node, nodes []Node, edges []Edge, runNodes []RunNode, now time.Time) {
	// Claim the node before anything else touches internal/tasks - the
	// loser of this CAS (another advance() racing the same run, see the
	// package doc) returns here without ever calling Submitter, so a node's
	// ticket is never submitted twice.
	claimed, err := r.Store.ClaimRunNode(rn.ID, now)
	if err != nil {
		log.Printf("dag: claiming node %s in run %s: %v", node.ID, runID, err)
		return
	}

	if !claimed {
		return
	}

	mu := r.desktopDispatchLock(node.DesktopID)
	mu.Lock()
	defer mu.Unlock()

	release := func() {
		if _, err := r.Store.ReleaseRunNode(rn.ID); err != nil {
			log.Printf("dag: releasing node %s in run %s: %v", node.ID, runID, err)
		}
	}

	if reason, err := r.Tasks.Busy(node.DesktopID); err != nil || reason != "" {
		if err != nil {
			log.Printf("dag: checking desktop %s busy for node %s in run %s: %v", node.DesktopID, node.ID, runID, err)
		}

		// Busy with something else right now - back off and retry next
		// tick rather than letting internal/tasks queue a second ticket
		// behind it (see TaskResults.Busy's doc).
		release()

		return
	}

	content := substitute(node.Content, node.ID, edges, nodes, runNodes)

	taskID, err := r.Submitter.SubmitTracked(ctx, node.DesktopID, content,
		tasks.Origin{Kind: tasks.OriginDAG, Ref: runID + "/" + node.ID})

	// The desktop became busy after the check above (a person sent it
	// something): the same "back off and retry next tick", not a failure.
	var busy interface{ DesktopBusy() string }
	if errors.As(err, &busy) {
		release()

		return
	}

	if err != nil {
		log.Printf("dag: dispatching node %s in run %s: %v", node.ID, runID, err)

		applied, markErr := r.Store.MarkRunNodeTerminal(rn.ID, NodeRunning, NodeFailed, "", now)
		if markErr != nil {
			log.Printf("dag: failing node %s in run %s: %v", node.ID, runID, markErr)
			return
		}

		if applied {
			r.skipDescendants(runID, node.ID, edges, now)
		}

		return
	}

	recorded, err := r.Store.SetRunNodeTaskID(rn.ID, taskID)
	if err != nil || !recorded {
		// The ticket is out but this run cannot follow it: failed with the task named, never left claimed.
		log.Printf("dag: recording dispatch of node %s in run %s (task %s): recorded=%v err=%v", node.ID, runID, taskID, recorded, err)
		r.fail(runID, rn, fmt.Sprintf(reasonUnrecorded, taskID), edges, now)
	}
}

// MarkStarted records that this process's Runner begins now, before anything can trigger a run: called by main
// before the Runner is handed to the HTTP routes, so a claim a Trigger makes before Run's goroutine starts is not
// taken for one a stopped control plane left (RW1-F-03). Run calls it too; only the first call counts.
func (r *Runner) MarkStarted() { r.markStarted(r.clock()) }

// markStarted records when this process's Runner began, once.
func (r *Runner) markStarted(at time.Time) {
	r.startedNS.CompareAndSwap(0, at.UnixNano())
}

// fail ends a running node as failed with reason, and skips what only it could reach.
func (r *Runner) fail(runID string, rn RunNode, reason string, edges []Edge, now time.Time) {
	applied, err := r.Store.MarkRunNodeTerminal(rn.ID, NodeRunning, NodeFailed, reason, now)
	if err != nil {
		log.Printf("dag: failing node %s in run %s: %v", rn.NodeID, runID, err)
		return
	}
	if applied {
		r.skipDescendants(runID, rn.NodeID, edges, now)
	}
}

// Cancel ends a run a person gave up on: every node not yet sent is skipped, every node in flight is failed as
// cancelled (its task, if one reached a desktop, is that desktop's to stop), and the run is closed as failed. It
// reports false when the run was not running.
func (r *Runner) Cancel(runID string) (bool, error) {
	now := r.clock()

	runNodes, err := r.Store.RunNodes(runID)
	if err != nil {
		return false, err
	}

	for _, rn := range runNodes {
		switch rn.Status {
		case NodePending:
			skipped, err := r.Store.SkipRunNode(rn.ID, now)
			if err != nil {
				return false, err
			}
			if !skipped {
				// A dispatch claimed it meanwhile: it is running now, so it is failed as cancelled like the others
				// (RW1-F-03), never left running in a closed run.
				if _, err := r.Store.MarkRunNodeTerminal(rn.ID, NodeRunning, NodeFailed, reasonCancelledBy, now); err != nil {
					return false, err
				}
			}
		case NodeRunning:
			if _, err := r.Store.MarkRunNodeTerminal(rn.ID, NodeRunning, NodeFailed, reasonCancelledBy, now); err != nil {
				return false, err
			}
		}
	}

	return r.Store.FinishRun(runID, RunFailed, now)
}

// skipDescendants marks Skipped every pending run-node reachable from a
// node that just failed - the default Argo/Airflow failure propagation. A
// node with several parents is doomed the moment any one of them fails
// (an AND join can never be satisfied once a required parent hasn't
// succeeded and never will), so this does not wait for its other parents to
// resolve first.
func (r *Runner) skipDescendants(runID, nodeID string, edges []Edge, now time.Time) {
	children := make(map[string][]string, len(edges))
	for _, e := range edges {
		children[e.FromNodeID] = append(children[e.FromNodeID], e.ToNodeID)
	}

	runNodes, err := r.Store.RunNodes(runID)
	if err != nil {
		log.Printf("dag: skipping descendants of %s in run %s: %v", nodeID, runID, err)
		return
	}

	byNodeID := make(map[string]RunNode, len(runNodes))
	for _, rn := range runNodes {
		byNodeID[rn.NodeID] = rn
	}

	queue := append([]string(nil), children[nodeID]...)

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		rn, ok := byNodeID[id]
		if !ok {
			continue
		}

		applied, err := r.Store.SkipRunNode(rn.ID, now)
		if err != nil {
			log.Printf("dag: skipping node %s in run %s: %v", id, runID, err)
			continue
		}

		if applied {
			queue = append(queue, children[id]...)
		}
	}
}

// closeIfDone finishes run as Succeeded or Failed once every run-node has
// reached a terminal state.
func closeIfDone(store *Store, runID string, runNodes []RunNode, now time.Time) {
	status := RunSucceeded

	for _, rn := range runNodes {
		switch rn.Status {
		case NodePending, NodeRunning:
			return
		case NodeFailed:
			status = RunFailed
		}
	}

	if _, err := store.FinishRun(runID, status, now); err != nil {
		log.Printf("dag: finishing run %s: %v", runID, err)
	}
}

// namedPlaceholder matches {{nodes.<name>.reply}} - always resolvable by
// name, regardless of the DAG's shape.
var namedPlaceholder = regexp.MustCompile(`\{\{nodes\.([A-Za-z0-9_-]+)\.reply\}\}`)

// parentPlaceholder is sugar for the sole parent's reply - only substituted
// when the node has exactly one incoming edge.
const parentPlaceholder = "{{parent.reply}}"

// substitute resolves a node's output placeholders against this run's
// run-nodes so far. A reference to a node that has not (yet, or ever)
// succeeded is left as the literal placeholder text rather than guessed at
// or erroring - pure and side-effect free, so it is computed fresh right
// before each dispatch rather than stored.
func substitute(content, nodeID string, edges []Edge, nodes []Node, runNodes []RunNode) string {
	nameByNodeID := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nameByNodeID[n.ID] = n.Name
	}

	replyByName := make(map[string]string, len(runNodes))
	for _, rn := range runNodes {
		if rn.Status != NodeSucceeded {
			continue
		}

		if name, ok := nameByNodeID[rn.NodeID]; ok {
			replyByName[name] = rn.FinalReply
		}
	}

	content = namedPlaceholder.ReplaceAllStringFunc(content, func(match string) string {
		name := namedPlaceholder.FindStringSubmatch(match)[1]

		reply, ok := replyByName[name]
		if !ok {
			return match
		}

		return reply
	})

	if strings.Contains(content, parentPlaceholder) {
		var parents []string
		for _, e := range edges {
			if e.ToNodeID == nodeID {
				parents = append(parents, e.FromNodeID)
			}
		}

		if len(parents) == 1 {
			if rn, ok := runNodeFor(runNodes, parents[0]); ok && rn.Status == NodeSucceeded {
				content = strings.ReplaceAll(content, parentPlaceholder, rn.FinalReply)
			}
		}
	}

	return content
}

func runNodeFor(runNodes []RunNode, nodeID string) (RunNode, bool) {
	for _, rn := range runNodes {
		if rn.NodeID == nodeID {
			return rn, true
		}
	}

	return RunNode{}, false
}
