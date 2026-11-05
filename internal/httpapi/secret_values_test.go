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

// After a restart, a desktop whose task was open keeps only hashes of its calls (its marked secrets are gone from
// memory) until that task ends; then its calls keep their text again.
func TestATaskOpenAtStartKeepsNoText(t *testing.T) {
	s := recordingServer(t)
	route, _ := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "box", Name: "box"}, "sign in for me", tasks.Origin{Kind: tasks.OriginPerson})
	s.ForgetSecretsAtStart()
	const v = "Tulip-31-Quay"
	recordTurn(t, s, map[string]any{"role": "assistant", "content": `Running: gui type e3 "` + v + `"`})
	if hits := blobsHolding(t, s, v); len(hits) > 0 {
		t.Fatalf("a call of a task open at start kept its text: %s", hits[0])
	}
	st, _ := s.LLMRec.Stats()
	if st.Calls != 1 {
		t.Fatalf("the call was not recorded: %+v", st)
	}
	finalWithVerdict(t, s, route.TaskID, "signed in", doneVerdict, true)
	recordTurn(t, s, map[string]any{"role": "assistant", "content": "the next task's words"})
	if hits := blobsHolding(t, s, "the next task's words"); len(hits) != 1 {
		t.Fatalf("after the task ended, calls still keep no text (%d)", len(hits))
	}
}

// The desktop's approval gate gets its own signed copy of a new request (never of a reply), and the values the
// records withheld, before the agent has the text (H-PY-5, H-PY-6). A marked request reaches it withheld.
func TestTheGateGetsTheRequestCopyAndTheWithheldValues(t *testing.T) {
	s := recordingServer(t)
	bodies := fakeAgentDesktop(t, s)
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"sign in to records.example.test, password: Plum7Orbit"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	all := bodies()
	copies, secrets := requestsWith(all, "text"), requestsWith(all, "secrets")
	// The gate keeps its copy on its disk: the withheld form, with the site's name kept and the value never.
	if len(copies) != 1 || copies[0]["text"] != "sign in to records.example.test, password: [withheld]" || copies[0]["sig"] == "" {
		t.Fatalf("the gate's copy = %v", copies)
	}
	if len(secrets) != 1 || len(secrets[0]["secrets"].([]any)) != 1 || secrets[0]["secrets"].([]any)[0] != "Plum7Orbit" {
		t.Fatalf("the gate's secrets = %v", secrets)
	}
	// The gate is told before the agent: its bodies come first.
	if _, isAgent := all[0]["content"]; isAgent {
		t.Fatalf("the agent had the request before the gate: %v", all)
	}

	askedQuestion(t, s, tasks.OutcomeQuery, "Which ledger?")
	before := len(requestsWith(bodies(), "text"))
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"purchases"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	if after := len(requestsWith(bodies(), "text")); after != before+1 { // the signed reply carries "text" too
		t.Fatalf("a reply sent %d bodies with text to the gate, want only its signed reply", after-before)
	}
	for _, b := range bodies() {
		if _, ok := b["asked"]; !ok && b["text"] == "purchases" {
			t.Fatalf("a reply was given to the gate as a request copy: %v", b)
		}
	}

	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"the vault code is Zq9-plum"}, "secret": {"1"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	copies = requestsWith(bodies(), "text")
	if last := copies[len(copies)-1]; last["text"] != "[secret withheld]" {
		t.Fatalf("a marked request reached the gate as %v", last)
	}
}

// A lesson cited from a judgement that holds no secret (an answer to "Which ledger?") still keeps none of a value the
// same task's request had withheld: the desktop's held values are the wall (review CP-A-2 N1).
func TestALessonKeepsNoHeldValueFromAnotherInput(t *testing.T) {
	s := recordingServer(t)
	fakeAgentDesktop(t, s)
	const v = "Plum7Orbit"
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"sign in to the staff portal as mira, password: " + v}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	task := askedQuestion(t, s, tasks.OutcomeQuery, "Which ledger?")
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"the purchases ledger"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	src := onlyJudgement(t, s, task)
	if src.Secret {
		t.Fatalf("an answer naming a ledger is secret: %+v", src)
	}
	rec := postLearned(t, s, "box", map[string]any{"task_id": task, "kind": "lesson", "derived_from": src.ID,
		"question": "Which ledger?", "text": "Bills go to purchases; mira signs in with " + v})
	if rec.Code != http.StatusCreated {
		t.Fatalf("learned: %d %s", rec.Code, rec.Body)
	}
	list, _ := s.Tasks.JudgementsForTask(task)
	for _, j := range list {
		if strings.Contains(j.Text, v) {
			t.Fatalf("judgement %s keeps the value: %q", j.ID, j.Text)
		}
	}
}

// A task open at start that is closed by a stop or a delete also ends its desktop's hashes-only recording.
func TestClosingTheOpenTasksEndsHashesOnly(t *testing.T) {
	s := recordingServer(t)
	if _, err := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "box", Name: "box"}, "sign in", tasks.Origin{Kind: tasks.OriginPerson}); err != nil {
		t.Fatal(err)
	}
	s.ForgetSecretsAtStart()
	if !s.secretsUnknown("box") {
		t.Fatal("a desktop with a task open at start records its calls with their text")
	}
	s.closeOpenTasks("box", "The desktop was stopped before this task finished.")
	if s.secretsUnknown("box") {
		t.Fatal("after its open tasks were closed, the desktop's calls still keep no text")
	}
}
