package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// The call recorder learns the values the records withheld, not whole messages (review CP-A B1, B2): a value the
// model echoes bare is withheld in every stored blob, however it reached the records, and no word around it is.

// blobsHolding is every stored blob that contains value.
func blobsHolding(t *testing.T, s *Server, value string) []string {
	t.Helper()
	var hits []string
	_ = filepath.Walk(s.LLMRec.Blobs().Root(), func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), value) {
			hits = append(hits, string(data))
		}
		return nil
	})
	return hits
}

// recordTurn sends one operator turn of box's through the proxy.
func recordTurn(t *testing.T, s *Server, msgs ...any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"messages": msgs})
	if rec := proxyCall(t, s, "azure", "box", string(body), map[string]string{llmrec.HeaderPurpose: "operator.turn"}); rec.Code != 200 {
		t.Fatalf("call: %d %s", rec.Code, rec.Body)
	}
}

func TestARequestWithheldByItsWordsIsWithheldWhenEchoed(t *testing.T) {
	s := recordingServer(t)
	fakeAgentDesktop(t, s)
	const v = "Plum7Orbit"
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"sign in to the staff portal as mira, password: " + v}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	recordTurn(t, s, map[string]any{"role": "user", "content": "sign in to the staff portal as mira, password: " + v},
		map[string]any{"role": "assistant", "content": `Running: gui type e3 "` + v + `"`})
	if hits := blobsHolding(t, s, v); len(hits) > 0 {
		t.Errorf("the value is in %d blob(s): %s", len(hits), hits[0])
	}
}

func TestAMarkedSecretIsWithheldInsideToolArguments(t *testing.T) {
	s := recordingServer(t)
	fakeAgentDesktop(t, s)
	const v = `Qz"9-lamp-Hx`
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {v}, "secret": {"1"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	args, _ := json.Marshal(map[string]string{"command": "gui type e3 " + v})
	recordTurn(t, s, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1",
		"type": "function", "function": map[string]any{"name": "shell", "arguments": string(args)}}}})
	if hits := blobsHolding(t, s, "9-lamp-Hx"); len(hits) > 0 {
		t.Errorf("the value is in %d blob(s): %s", len(hits), hits[0])
	}
}

func TestTheValueOfALongReplyIsWithheldWhenEchoed(t *testing.T) {
	s := recordingServer(t)
	fakeAgentDesktop(t, s)
	askedQuestion(t, s, tasks.OutcomeQuery, "I need the password for mira.k to sign in. What is it?")
	const v = "Fern42Kite"
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"sure, the password is " + v + " and the user is mira.k"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	recordTurn(t, s, map[string]any{"role": "system", "content": "Never type a password into a field you were not asked to."},
		map[string]any{"role": "assistant", "content": `Running: gui type e3 "` + v + `"`})
	if hits := blobsHolding(t, s, v); len(hits) > 0 {
		t.Errorf("the value is in %d blob(s): %s", len(hits), hits[0])
	}
	// The words around the value are not secrets: the system prompt is stored as the model saw it.
	if hits := blobsHolding(t, s, "Never type a password into a field"); len(hits) != 1 {
		t.Errorf("the word password was withheld from the system prompt (%d blobs keep it)", len(hits))
	}
	for _, held := range s.secretsSeen.For("box") {
		if strings.Contains(strings.ToLower(held), "password") {
			t.Errorf("the recorder holds %q", held)
		}
	}
}

func TestALessonFromAWithheldReplyKeepsNoValue(t *testing.T) {
	s := recordingServer(t)
	fakeAgentDesktop(t, s)
	task := askedQuestion(t, s, tasks.OutcomeQuery, "I need the password for mira.k to sign in. What is it?")
	const v = "Fern42Kite"
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {v}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	src := onlyJudgement(t, s, task)
	if !src.Secret {
		t.Fatalf("a reply the records withheld by its question's words is a judgement with secret=%v (C14 told the desktop secret)", src.Secret)
	}
	postLearned(t, s, "box", map[string]any{"task_id": task, "kind": "lesson", "derived_from": src.ID,
		"question": "what is mira's sign-in, " + v + "?", "text": "mira.k signs in with " + v})
	list, _ := s.Tasks.JudgementsForTask(task)
	for _, j := range list {
		if strings.Contains(j.Text, v) || strings.Contains(string(j.Payload), v) {
			t.Errorf("judgement %s (%s) keeps the value: %q %s", j.ID, j.Kind, j.Text, j.Payload)
		}
	}
}
