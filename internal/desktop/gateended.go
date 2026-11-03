package desktop

import (
	"context"
	"errors"
	"log"
	"sync"
)

// A task's secret values stay in the approval gate's own store (gate_server.keep_secrets) until the gate is told the
// task ended: the gate cannot tell an open task from an ended one by counting requests (review PY-A-2 R2-B2). The
// control plane tells it, signed under its own label, whenever a task closes (done, error, stopped, deleted,
// abandoned). Telling it never holds up the close: the notice is kept and sent in the background, and one the gate
// did not take is sent again before the next thing the control plane gives that gate.

// maxPendingTaskEnded bounds the notices kept for one desktop whose gate is not taking them (stopped, or a gate from
// before the route). Past it the oldest is let go and logged: its values then stay in the gate until the gate's own
// bound lets them go, which only withholds more.
const maxPendingTaskEnded = 256

// pendingTaskEnded is the notices not yet taken, per desktop, oldest first.
type pendingTaskEnded struct {
	mu    sync.Mutex
	tasks map[string][]string
}

// SignGateTaskEnded is the signature on a notice that a task ended (/task-ended): its own label, over the task, the
// time and a one-use nonce, so it can stand for no reply, request or list of values (gate_server.sign_task_ended).
func SignGateTaskEnded(key, taskID, ts, nonce string) string {
	return gateMAC(key, "task-ended", taskID, ts, nonce)
}

// GateTaskEnded tells desktop id's approval gate that task taskID has ended, so the gate lets go of that task's
// secret values. It never blocks and never fails the close: the notice is sent in the background, and one the gate
// does not take is logged and sent again with the next delivery to that gate.
func (m *Manager) GateTaskEnded(id, taskID string) {
	if m == nil || taskID == "" {
		return
	}
	id = sanitizeName(id)
	if id == "" {
		return
	}
	m.queueTaskEnded(id, taskID)
	go m.flushTaskEnded(context.Background(), id)
}

// queueTaskEnded keeps a notice for desktop id's gate until the gate takes it, within maxPendingTaskEnded.
func (m *Manager) queueTaskEnded(id, taskID string) {
	m.taskEnded.mu.Lock()
	if m.taskEnded.tasks == nil {
		m.taskEnded.tasks = map[string][]string{}
	}
	queue := append(m.taskEnded.tasks[id], taskID)
	if len(queue) > maxPendingTaskEnded {
		log.Printf("desktop %s: %d task-ended notices its gate has not taken; letting go of the oldest (task %s)", id, len(queue), queue[0])
		queue = queue[len(queue)-maxPendingTaskEnded:]
	}
	m.taskEnded.tasks[id] = queue
	m.taskEnded.mu.Unlock()
}

// flushTaskEnded sends desktop id's gate every task-ended notice it has not taken, oldest first, stopping at the first
// it does not take (the rest wait for the next delivery). A desktop Docker no longer has takes none: its gate went
// with it, so its notices are let go.
func (m *Manager) flushTaskEnded(ctx context.Context, id string) {
	m.taskEnded.mu.Lock()
	queue := append([]string(nil), m.taskEnded.tasks[id]...)
	m.taskEnded.mu.Unlock()
	for _, taskID := range queue {
		err := m.postToGateNow(ctx, id, "/task-ended", func(key, ts, nonce string) map[string]any {
			return map[string]any{"task_id": taskID, "ts": ts, "nonce": nonce, "sig": SignGateTaskEnded(key, taskID, ts, nonce)}
		})
		switch {
		case err == nil:
			m.takenTaskEnded(id, taskID)
		case errors.Is(err, errDesktopGone):
			m.forgetTaskEnded(id)
			return
		default:
			log.Printf("desktop %s: the approval gate was not told that task %s ended; sending it again with the next delivery: %v", id, taskID, err)
			return
		}
	}
}

// takenTaskEnded removes one notice the gate took.
func (m *Manager) takenTaskEnded(id, taskID string) {
	m.taskEnded.mu.Lock()
	defer m.taskEnded.mu.Unlock()
	queue := m.taskEnded.tasks[id]
	for i, t := range queue {
		if t == taskID {
			queue = append(queue[:i:i], queue[i+1:]...)
			break
		}
	}
	if len(queue) == 0 {
		delete(m.taskEnded.tasks, id)
		return
	}
	m.taskEnded.tasks[id] = queue
}

// forgetTaskEnded lets go of every notice for a desktop that is gone.
func (m *Manager) forgetTaskEnded(id string) {
	m.taskEnded.mu.Lock()
	delete(m.taskEnded.tasks, id)
	m.taskEnded.mu.Unlock()
}

// pendingTaskEndedFor is the notices desktop id's gate has not taken, oldest first.
func (m *Manager) pendingTaskEndedFor(id string) []string {
	m.taskEnded.mu.Lock()
	defer m.taskEnded.mu.Unlock()
	return append([]string(nil), m.taskEnded.tasks[sanitizeName(id)]...)
}
