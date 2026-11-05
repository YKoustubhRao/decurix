package llm

import (
	"context"
	"regexp"
	"strconv"
	"sync/atomic"
)

// Tags name what a model call was made for beyond its purpose: the control plane's task, soul's job, and the
// operator's turn ("<job>:<n>"). They ride on the call's context and leave soul only as HTTP headers (TagHeaders) and
// in soul's trace, never in a request body: the prompt prefix, and with it the provider's cache and Ollama's loaded
// prefix, stays byte-identical whatever the tags are (PLAN-P2 C1, C2).
type Tags struct{ Task, Job, Turn string }

// TaskIDShape is the form of a control-plane task id soul accepts (PLAN-P2 C2); anything else is ignored.
var TaskIDShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// turnShape is the proxy's accepted form of a turn (PLAN-P2 C1).
var turnShape = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,64}$`)

// Caller is who soul is to the proxy (X-Decurix-Caller).
const Caller = "soul"

// The headers the proxy reads (PLAN-P2 C1).
const (
	HeaderPurpose = "X-Decurix-Purpose"
	HeaderTask    = "X-Decurix-Task"
	HeaderTurn    = "X-Decurix-Turn"
	HeaderCaller  = "X-Decurix-Caller"
)

type tagsKey struct{}

// tagged is what the context carries: the tags, and the job's turn counter (shared by every context derived from the
// job's, so two operator runs in one job never number a turn twice).
type tagged struct {
	Tags
	turns *atomic.Int64
}

// WithTags returns ctx carrying t, with a fresh turn counter for t's job. A task that is not of TaskIDShape is
// dropped; t.Turn is kept only if it is of the proxy's turn form.
func WithTags(ctx context.Context, t Tags) context.Context {
	if !TaskIDShape.MatchString(t.Task) {
		t.Task = ""
	}
	if t.Turn != "" && !turnShape.MatchString(t.Turn) {
		t.Turn = ""
	}
	return context.WithValue(ctx, tagsKey{}, tagged{Tags: t, turns: new(atomic.Int64)})
}

// TagsFrom is the tags ctx carries (zero when none).
func TagsFrom(ctx context.Context) Tags {
	if ctx == nil {
		return Tags{}
	}
	if t, ok := ctx.Value(tagsKey{}).(tagged); ok {
		return t.Tags
	}
	return Tags{}
}

// NextTurn returns ctx for the job's next turn: its Turn is "<job>:<n>", n counting from 1 across the whole job.
// Without a job in ctx it is ctx unchanged.
func NextTurn(ctx context.Context) context.Context {
	t, ok := ctx.Value(tagsKey{}).(tagged)
	if !ok || t.Job == "" || t.turns == nil {
		return ctx
	}
	turn := t.Job + ":" + strconv.FormatInt(t.turns.Add(1), 10)
	if !turnShape.MatchString(turn) {
		return ctx
	}
	t.Turn = turn
	return context.WithValue(ctx, tagsKey{}, t)
}

// TagHeaders are the proxy's headers for one call made under ctx for purpose: the purpose (PurposeUntagged when none),
// the task and turn when known, and the caller.
func TagHeaders(ctx context.Context, purpose string) map[string]string {
	if purpose == "" || !purposeShape.MatchString(purpose) {
		purpose = PurposeUntagged
	}
	h := map[string]string{HeaderPurpose: purpose, HeaderCaller: Caller}
	t := TagsFrom(ctx)
	if t.Task != "" {
		h[HeaderTask] = t.Task
	}
	if t.Turn != "" {
		h[HeaderTurn] = t.Turn
	}
	return h
}
