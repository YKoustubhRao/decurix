package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"thebot/internal/learnreview"
	"thebot/internal/llm"
)

// reviewBody is a review call for task with more fields set.
func reviewBody(t *testing.T, task string, more map[string]any) string {
	t.Helper()
	body := map[string]any{"brain": "hosted", "purpose": reviewPurpose, "task": task, "max_tokens": 800,
		"timeout_ms": 30000, "secrets": []string{}}
	for k, v := range more {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// keptRun is a finished goal's transcript, kept for task as the operator keeps it.
func keptRun(s *server, task string) []llm.Message {
	msgs := []llm.Message{
		{Role: "system", Content: "You are the worker.\nLesson (worker): Totals are shown with two decimals."},
		{Role: "user", Content: "Work in /home/w. Request: send the weekly summary"},
		{Role: "assistant", Content: "$ office read summary.xlsx"},
		{Role: "user", Content: "exit 0\nThe person replied: always put the region column first"},
		{Role: "assistant", Content: "DONE: sent"},
	}
	ctx := learnreview.With(llm.WithTags(context.Background(), llm.Tags{Task: task, Job: "job1"}), s.reviews)
	learnreview.Keep(ctx, "m", msgs, 2, "send the weekly summary")
	return msgs
}

func TestTheReviewIsTheRunsOwnPrefixWithOneInstructionAndItsOwnPurpose(t *testing.T) {
	s, proxy, _ := callServer(t)
	msgs := keptRun(s, "t_rev1")
	proxy.answer = `{"proposals": [{"kind": "lesson", "text": "The region column comes first in the summary.", "why": "the person said so", "evidence": ["always put the region column first"]}, {"kind": "lesson", "text": "x", "why": "", "evidence": ["made up words"]}]}`

	// The run's own last turn, as the operator sent it: the bytes the review must open with.
	if _, err := s.provider.Chat(llm.WithTags(context.Background(), llm.Tags{Task: "t_rev1", Job: "job1"}), "m",
		msgs, llm.Options{Purpose: llm.PurposeOperatorTurn}); err != nil {
		t.Fatal(err)
	}
	rec := postCall(s, testAgentToken, llmReviewPath, reviewBody(t, "t_rev1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body.String())
	}
	if len(proxy.bodies) != 2 {
		t.Fatalf("calls = %d", len(proxy.bodies))
	}
	turn, review := proxy.bodies[0]["messages"].([]any), proxy.bodies[1]["messages"].([]any)
	if len(review) != len(turn)+1 || !reflect.DeepEqual(review[:len(turn)], turn) {
		t.Fatalf("the review does not open with the run's messages unchanged:\n%v\n%v", turn, review)
	}
	if proxy.bodies[0]["prompt_cache_key"] != proxy.bodies[1]["prompt_cache_key"] {
		t.Fatal("the review goes to another prompt cache than the run")
	}
	if got := proxy.headers[1].Get("X-Decurix-Purpose"); got != reviewPurpose {
		t.Fatalf("purpose header = %q", got)
	}
	if got := proxy.headers[1].Get("X-Decurix-Task"); got != "t_rev1" {
		t.Fatalf("task header = %q", got)
	}

	var answer struct {
		Usage    map[string]int       `json:"usage"`
		Decision learnreview.Decision `json:"decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	d := answer.Decision
	if d.Source != "transcript" || len(d.Proposals) != 2 || !d.Proposals[0].InRun[0] || d.Proposals[1].InRun[0] {
		t.Fatalf("decision = %+v", d)
	}
	if answer.Usage["cached_tokens"] != 100 {
		t.Fatalf("usage = %v", answer.Usage)
	}
}

func TestAReviewWithNoTranscriptReadsTheJobLog(t *testing.T) {
	s, proxy, _ := callServer(t)
	job := newJobRecord("j9", "send the weekly summary")
	job.TaskID = "t_rev2"
	job.appendLog("$ office read summary.xlsx -> exit 0 · region, total")
	job.FinalAnswer = "Sent."
	job.startedAt = time.Now()
	job.finish()
	s.jobs["j9"] = job
	proxy.answer = `{"proposals": [{"kind": "app_note", "text": "The summary sheet opens with its region column.", "why": "seen", "evidence": ["region, total"]}]}`

	rec := postCall(s, testAgentToken, llmReviewPath, reviewBody(t, "t_rev2", map[string]any{"notes": "Asked: which order? Said: region first"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body.String())
	}
	var answer struct {
		Decision learnreview.Decision `json:"decision"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if answer.Decision.Source != "job_log" || len(answer.Decision.Proposals) != 1 || !answer.Decision.Proposals[0].InRun[0] {
		t.Fatalf("decision = %+v", answer.Decision)
	}
	if got := proxy.headers[0].Get("X-Decurix-Purpose"); got != reviewPurpose {
		t.Fatalf("purpose header = %q", got)
	}
}

func TestTheReviewRouteRefusesWhatIsNotAReview(t *testing.T) {
	s, proxy, _ := callServer(t)
	keptRun(s, "t_rev3")
	for name, body := range map[string]string{
		"another purpose": reviewBody(t, "t_rev3", map[string]any{"purpose": "py.judge"}),
		"messages":        reviewBody(t, "t_rev3", map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}}),
		"no task":         reviewBody(t, "", nil),
		"a local brain":   reviewBody(t, "t_rev3", map[string]any{"brain": "local"}),
	} {
		if rec := postCall(s, testAgentToken, llmReviewPath, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := postCall(s, testAgentToken, llmReviewPath, reviewBody(t, "t_unknown", nil)); rec.Code != http.StatusConflict {
		t.Errorf("a task soul keeps nothing of: %d", rec.Code)
	}
	// The call key opens model calls only, never a review.
	if rec := postCall(s, "call-key-for-tests", llmReviewPath, reviewBody(t, "t_rev3", nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the call key: %d", rec.Code)
	}
	if len(proxy.bodies) != 0 {
		t.Fatalf("a refused review reached the model %d times", len(proxy.bodies))
	}
}
