// Package learnreview is the post-task review pass (docs/learning-loop.md, "The review pass"): after a task the final
// check judged done, the model that did the work reads its own transcript once more, with one instruction appended,
// and proposes what a later task should know. It only proposes: the Python agent checks each proposal in code and
// stages it on the control plane, and nothing is learned until a person approves it there.
//
// This file keeps each task's last operator transcript in memory, so the review can be sent the very messages the
// task's last turn was sent (a prefix the provider has cached) with one more message after them.
package learnreview

import (
	"context"
	"sync"
	"time"

	"thebot/internal/llm"
)

// Bounds on what the keeper holds: the last few tasks' transcripts, and at most so many bytes in all (pictures count).
const (
	maxKeptTasks = 4
	maxKeptBytes = 48 << 20
)

// Kept is one task's last operator transcript, as its last model turn was sent, and what it needs to be read again.
type Kept struct {
	Task string
	// Job is the soul job whose goal left this transcript.
	Job string
	// Model is the model the goal's turns asked for: the review asks the same one, so its cache is the same.
	Model string
	// Messages is the transcript, the system frame first.
	Messages []llm.Message
	// PrefixLen is how many messages open the transcript before any command ran (the frame and the request).
	PrefixLen int
	// Request is the goal's request, as the operator was given it.
	Request string
	At      time.Time
}

// size is roughly how much memory k holds.
func (k Kept) size() int {
	n := len(k.Request)
	for _, m := range k.Messages {
		n += len(m.Content)
		for _, img := range m.Images {
			n += len(img)
		}
	}
	return n
}

// Keeper holds the last transcript of a few tasks, newest kept.
type Keeper struct {
	mu    sync.Mutex
	kept  map[string]Kept
	order []string // task ids, oldest first
}

// NewKeeper is an empty keeper.
func NewKeeper() *Keeper { return &Keeper{kept: map[string]Kept{}} }

// put keeps k as its task's transcript, dropping the oldest tasks past the bounds. A transcript larger than the whole
// bound is not kept (the review then reads the job's log instead).
func (kp *Keeper) put(k Kept) {
	if kp == nil || k.Task == "" || len(k.Messages) == 0 || k.size() > maxKeptBytes {
		return
	}
	kp.mu.Lock()
	defer kp.mu.Unlock()
	kp.drop(k.Task)
	kp.kept[k.Task] = k
	kp.order = append(kp.order, k.Task)
	total := 0
	for _, t := range kp.order {
		total += kp.kept[t].size()
	}
	for len(kp.order) > 1 && (len(kp.order) > maxKeptTasks || total > maxKeptBytes) {
		total -= kp.kept[kp.order[0]].size()
		kp.drop(kp.order[0])
	}
}

// drop forgets task. Called with mu held.
func (kp *Keeper) drop(task string) {
	delete(kp.kept, task)
	for i, t := range kp.order {
		if t == task {
			kp.order = append(kp.order[:i], kp.order[i+1:]...)
			break
		}
	}
}

// Get is task's last transcript, a copy the caller may append to, and false when none is kept.
func (kp *Keeper) Get(task string) (Kept, bool) {
	if kp == nil || task == "" {
		return Kept{}, false
	}
	kp.mu.Lock()
	defer kp.mu.Unlock()
	k, ok := kp.kept[task]
	if !ok {
		return Kept{}, false
	}
	k.Messages = append([]llm.Message(nil), k.Messages...)
	return k, true
}

// keeperKey carries a job's keeper on its context.
type keeperKey struct{}

// With returns ctx carrying kp: the operator's goals run under it keep their transcripts there.
func With(ctx context.Context, kp *Keeper) context.Context {
	return context.WithValue(ctx, keeperKey{}, kp)
}

// Keep files a goal's transcript under the task ctx is tagged with (llm.WithTags), in the keeper ctx carries. Nothing
// is kept without a keeper or a task. The messages are copied: the goal may go on changing its own (a paused goal
// resumed later), and the later state replaces this one when it returns.
func Keep(ctx context.Context, model string, messages []llm.Message, prefixLen int, request string) {
	kp, _ := ctx.Value(keeperKey{}).(*Keeper)
	tags := llm.TagsFrom(ctx)
	if kp == nil || tags.Task == "" {
		return
	}
	kp.put(Kept{Task: tags.Task, Job: tags.Job, Model: model, Messages: append([]llm.Message(nil), messages...),
		PrefixLen: prefixLen, Request: request, At: time.Now()})
}
