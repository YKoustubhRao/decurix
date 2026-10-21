package httpapi

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// finishedKeep is how many finished tasks the feed remembers: enough for a page that polls every few seconds.
const finishedKeep = 50

// FinishedTask is one task that ended with its work done, as the pages' "it is done" notification tells it.
type FinishedTask struct {
	Seq       int64     `json:"seq"`
	DesktopID string    `json:"desktop_id"`
	VMName    string    `json:"vm_name"`
	Task      string    `json:"task"`
	Message   string    `json:"message"`
	At        time.Time `json:"at"`
}

// finishedFeed is the last few tasks that finished. A finished task leaves no query note (only a question, an approval
// or a failure does), so without it nothing could tell a person, on another page or another tab, that the work they
// sent is done. Memory only: a page just loaded starts from the latest and never replays old ones.
type finishedFeed struct {
	mu    sync.Mutex
	seq   int64
	items []FinishedTask
}

func (f *finishedFeed) add(t FinishedTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	t.Seq = f.seq
	t.At = time.Now()
	f.items = append(f.items, t)
	if len(f.items) > finishedKeep {
		f.items = append([]FinishedTask(nil), f.items[len(f.items)-finishedKeep:]...)
	}
}

func (f *finishedFeed) since(after int64) (int64, []FinishedTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []FinishedTask{}
	for _, t := range f.items {
		if t.Seq > after {
			out = append(out, t)
		}
	}
	return f.seq, out
}

// recordFinished adds a task that just ended done to the feed, named by the request that started it.
func (s *Server) recordFinished(desktopID, vmName, taskID, message string) {
	task := ""
	if taskID != "" && s.Tasks != nil {
		if d, err := s.Tasks.Get(taskID); err == nil {
			task = d.Request
		}
	}
	if vmName == "" {
		vmName = desktopID
	}
	s.finished.add(FinishedTask{DesktopID: desktopID, VMName: vmName, Task: clip(task, 200), Message: clip(message, 400)})
}

// finishedTasksHandler answers GET /api/tasks/finished?after=<seq>: the tasks finished since that one, and the latest
// seq to ask after next time. A page's first call (no after) gets only the latest seq.
func (s *Server) finishedTasksHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	raw := r.URL.Query().Get("after")
	after, err := strconv.ParseInt(raw, 10, 64)
	latest, items := s.finished.since(after)
	if raw == "" || err != nil {
		items = []FinishedTask{}
	}
	jsonResponse(w, map[string]any{"latest": latest, "items": items})
}
