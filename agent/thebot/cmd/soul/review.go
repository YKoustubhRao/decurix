package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"thebot/internal/learnreview"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/trace"
	"thebot/internal/withheld"
)

// /api/llm/review is the post-task review pass (internal/learnreview; docs/learning-loop.md "The review pass"): after a
// task the final check judged done, the agent asks soul to read the task's run once more and propose what a later task
// should know. Soul sends the task's last operator transcript exactly as its last turn was sent, with one instruction
// appended as the newest message, so the provider reads all but that message from its prompt cache; it reads the
// answer and marks which evidence quotes stand in what the run read. It proposes only: nothing here keeps anything.
//
// When the transcript is no longer kept (soul restarted, or later tasks pushed it out), the review is made from the
// task's job records instead - each job's request, its log and its report - as a fresh two-message call ("job_log").
//
// The agent token opens it, never soul's call key (guard.go). The call carries the agent's purpose (py.learn.review)
// and the task's tags, so the proxy books it to the task under its own purpose; it is added to no job's usage.
//
// llmReviewPath is the route.
const llmReviewPath = "/api/llm/review"

// reviewPurpose is the one purpose a review is made under.
const reviewPurpose = "py.learn.review"

// maxReviewNotes bounds the agent's notes for a review made from the job log (the person's words on the task).
const maxReviewNotes = 16 << 10

// maxReviewLogLines bounds how many of a job's last log lines a review made from the job log reads.
const maxReviewLogLines = 400

// reviewCallRequest is a review: a call's fields (brain, purpose, task, limits, secrets) with no messages - soul has
// the run - and, for a review made from the job log, the agent's notes of the person's words on the task.
type reviewCallRequest struct {
	llmCallRequest
	Notes string `json:"notes"`
}

// handleLLMReview makes the review of a finished task and answers the call with soul's reading of it.
func (s *server) handleLLMReview(w http.ResponseWriter, r *http.Request) {
	var req reviewCallRequest
	if !decodeCall(w, r, &req) {
		return
	}
	refuse := func(status int, why string) {
		writeJSON(w, status, map[string]any{"error": why, "where": "soul"})
	}
	switch {
	case len(req.Messages) > 0:
		refuse(http.StatusBadRequest, "a review is made from the run soul kept, not from messages")
		return
	case req.Purpose != reviewPurpose:
		refuse(http.StatusBadRequest, "a review's purpose is "+reviewPurpose)
		return
	case !llm.TaskIDShape.MatchString(req.Task):
		refuse(http.StatusBadRequest, "a review names the task it reviews")
		return
	case len(req.Notes) > maxReviewNotes:
		refuse(http.StatusBadRequest, "the notes are too long")
		return
	}
	// The call's own checks (brain, limits, secrets, model), made on a stand-in message: the messages are soul's own.
	probe := req.llmCallRequest
	probe.Messages = []callMessage{{Role: "user", Content: []byte(`"review"`)}}
	model, _, opts, timeout, err := s.callOf(probe)
	if err != nil {
		refuse(http.StatusBadRequest, err.Error())
		return
	}

	messages, sources, from, job := s.reviewMessages(req.Task, req.Notes)
	if len(messages) == 0 {
		refuse(http.StatusConflict, "soul keeps nothing of that task's run")
		return
	}
	if from == "transcript" {
		if kept, ok := s.reviews.Get(req.Task); ok && kept.Model != "" {
			model = kept.Model // the model the run's turns asked for: the same cache
		}
	}
	secrets := withheld.Clean(req.Secrets)
	ctx := llm.WithTags(r.Context(), llm.Tags{Task: req.Task, Job: job})
	ctx = withheld.With(trace.WithWriter(ctx, s.tracer), secrets)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := s.provider.Chat(ctx, model, messages, opts)
	if err != nil {
		status, why := callFailure(err)
		why = memory.RedactSecrets(secrets.Text(why))
		log.Printf("[soul] the review of a task failed: HTTP %d", status)
		writeJSON(w, status, map[string]any{"error": why, "where": "provider"})
		return
	}
	decision := learnreview.Decide(res.Text)
	learnreview.Ground(&decision, sources)
	decision.Source = from
	// The task's secret values never leave soul in what it read of the answer.
	for i := range decision.Proposals {
		p := &decision.Proposals[i]
		p.Text, p.Why, p.Replaces = secrets.Text(p.Text), secrets.Text(p.Why), secrets.Text(p.Replaces)
		for j := range p.Evidence {
			p.Evidence[j] = secrets.Text(p.Evidence[j])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text": secrets.Text(res.Text), "truncated": res.Truncated, "model": model, "provider": s.provider.Name(),
		"usage": map[string]int{"prompt_tokens": res.PromptTokens, "completion_tokens": res.ResponseTokens,
			"cached_tokens": res.CachedTokens},
		"decision": decision,
	})
}

// reviewMessages is what the review of task is sent, what its quotes may stand in, what it was made from
// ("transcript" or "job_log") and the job it reads. Nothing when soul keeps nothing of the task.
func (s *server) reviewMessages(task, notes string) ([]llm.Message, []string, string, string) {
	if kept, ok := s.reviews.Get(task); ok {
		return learnreview.Messages(kept), learnreview.Sources(kept), "transcript", kept.Job
	}
	jobs := s.jobsOfTask(task)
	if len(jobs) == 0 {
		return nil, nil, "", ""
	}
	var b strings.Builder
	var sources []string
	for i, j := range jobs {
		j.mu.Lock()
		prompt, answer := j.secrets.Text(j.Prompt), j.secrets.Text(j.FinalAnswer)
		lines := append([]string(nil), j.Log...) // withheld as they were logged (appendLog)
		j.mu.Unlock()
		if len(lines) > maxReviewLogLines {
			lines = lines[len(lines)-maxReviewLogLines:]
		}
		fmt.Fprintf(&b, "=== Job %d\nRequest:\n%s\n\nLog:\n%s\n\nReport:\n%s\n\n", i+1, prompt, strings.Join(lines, "\n"), answer)
		sources = append(sources, prompt)
		sources = append(sources, lines...)
	}
	if notes = strings.TrimSpace(notes); notes != "" {
		fmt.Fprintf(&b, "=== What the person said on this task\n%s\n\n", notes)
	}
	b.WriteString(learnreview.Instruction)
	return []llm.Message{{Role: "system", Content: learnreview.FallbackSystem}, {Role: "user", Content: b.String()}},
		sources, "job_log", jobs[len(jobs)-1].ID
}

// jobsOfTask is the finished job records of task this soul still holds, oldest first.
func (s *server) jobsOfTask(task string) []*jobRecord {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	var out []*jobRecord
	for _, j := range s.jobs {
		if j.TaskID == task && j.isDone() {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].startedAt.Before(out[b].startedAt) })
	return out
}
