package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/stepimages"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// chatServer builds the slice of Server the chat handlers actually use. They
// never touch the Docker manager — reaching a desktop is sendRequestHandler's
// job, not theirs — so this needs no daemon and no containers.
func chatServer(t *testing.T) *Server {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	open := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{}

	s.QueryNotes, err = querynotes.NewStore(db)
	open(err)
	s.Chat, err = conversation.NewStore(db)
	open(err)
	s.StepImages, err = stepimages.NewStore(db, 0)
	open(err)
	s.Tasks, err = tasks.NewStore(db)
	open(err)

	return s
}

func postJSON(t *testing.T, handler http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler(rec, req)

	return rec
}

func readThread(t *testing.T, s *Server, id string) conversation.Thread {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/desktops/chat?id="+id, nil)
	rec := httptest.NewRecorder()

	s.chatHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("chatHandler: got status %d", rec.Code)
	}

	var thread conversation.Thread

	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decoding thread: %v", err)
	}

	return thread
}

// A whole turn as the control plane sees it: the agent reports steps while it
// works, then reports the answer, which closes the turn and takes the steps
// with it.
func TestAgentTurnBuildsTranscript(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"$ ls -la /work"}`)
	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"exit 0"}`)

	thread := readThread(t, s, "research-box")

	if !thread.Running {
		t.Fatal("expected reported steps to show the desktop as working")
	}

	if len(thread.Steps) != 2 {
		t.Fatalf("expected 2 live steps, got %d", len(thread.Steps))
	}

	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"research-box","vm_name":"research-box","filename":"a.txt",
		  "kind":"final","reply":"Cleaned up /work.",
		  "content":"DECURIX TASK COMPLETED\n\nFinal reply:\nCleaned up /work.\n\nCompleted at:\nnow"}`)

	thread = readThread(t, s, "research-box")

	if thread.Running {
		t.Fatal("expected the agent's answer to close the turn")
	}

	if len(thread.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(thread.Entries))
	}

	entry := thread.Entries[0]

	// The chat shows what the agent said, not the note's boilerplate — the
	// Query rail is what keeps the formatted version.
	if entry.Content != "Cleaned up /work." {
		t.Fatalf("expected just the agent's words, got %q", entry.Content)
	}

	if entry.Kind != conversation.KindEntry {
		t.Fatalf("expected a plain entry, got kind %q", entry.Kind)
	}

	if len(entry.Steps) != 2 {
		t.Fatalf("expected the answer to carry its 2 steps, got %d", len(entry.Steps))
	}

	// Successful replies belong in chat, not the actionable Query rail.
	if notes := s.QueryNotes.List(); len(notes) != 0 {
		t.Fatalf("expected no query for a successful reply, got %d", len(notes))
	}
}

// An on-screen action arrives as a step carrying its before/after
// screenshots: both land in the image store, and the step links to them.
func TestActionStepKeepsItsScreenshots(t *testing.T) {
	s := chatServer(t)

	shot := base64.StdEncoding.EncodeToString(tinyPNG(t))

	postJSON(t, s.agentEventHandler, "/api/agent/events", `{"desktop_id":"Web.Box","text":"click button \"Save\" - settings saved",
		"action":{"id":"20260913-ab","kind":"click","target":"button \"Save\" (e4)","backend":"cdp","outcome":"done","change":"status appeared"},
		"before_png":"`+shot+`","after_png":"`+shot+`"}`)

	thread := readThread(t, s, "web-box")

	if len(thread.Steps) != 1 || thread.Steps[0].Action == nil {
		t.Fatalf("expected one action step, got %+v", thread.Steps)
	}

	action := thread.Steps[0].Action

	if action.Kind != "click" || action.Outcome != "done" || action.RecordID != "20260913-ab" {
		t.Fatalf("action fields not kept: %+v", action)
	}

	for _, url := range []string{action.Before, action.After} {
		if !strings.HasPrefix(url, stepImagePath+"?id=") {
			t.Fatalf("expected a step-image URL, got %q", url)
		}

		rec := httptest.NewRecorder()
		s.stepImageHandler(rec, httptest.NewRequest(http.MethodGet, url, nil))

		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatalf("GET %s: status %d, type %q", url, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// A screenshot that is not an image loses the picture, never the step.
func TestActionStepSurvivesAnUnusableScreenshot(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.agentEventHandler, "/api/agent/events", `{"desktop_id":"box","text":"click",
		"action":{"kind":"click","outcome":"needs_approval"},"before_png":"bm90IGEgcG5n"}`)

	thread := readThread(t, s, "box")

	if len(thread.Steps) != 1 || thread.Steps[0].Action == nil || thread.Steps[0].Action.Before != "" {
		t.Fatalf("expected the step without a screenshot, got %+v", thread.Steps)
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 6))); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// A desktop built before kind/reply existed sends only the formatted note,
// so the handler has to recover both from its text.
func TestLegacyCallbackIsParsed(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"research-box","vm_name":"research-box","filename":"b.txt",
		  "content":"DECURIX QUERY REQUEST\n\nTask:\nwire money\n\nProblem:\nMay I send the payment?\n\nQuery ID:\nabc"}`)

	thread := readThread(t, s, "research-box")

	if len(thread.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(thread.Entries))
	}

	entry := thread.Entries[0]

	if entry.Content != "May I send the payment?" {
		t.Fatalf("expected the problem text alone, got %q", entry.Content)
	}

	if entry.Kind != conversation.KindApproval {
		t.Fatalf("expected a query note to read as an approval, got kind %q", entry.Kind)
	}
}

// A desktop's own spelling of its ID and the dashboard's spelling must land on
// the same thread, or its entries belong to a desktop nobody is looking at.
func TestDesktopIDSpellingIsNormalised(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"My.Box","text":"hello"}`)

	if got := len(readThread(t, s, "my-box").Steps); got != 1 {
		t.Fatalf("expected My.Box and my-box to be one thread, got %d steps", got)
	}
}

// Progress reporting must never be able to fail a task, so an unusable event
// is accepted and dropped rather than answered with an error.
func TestMalformedEventIsAccepted(t *testing.T) {
	s := chatServer(t)

	rec := postJSON(t, s.agentEventHandler, "/api/agent/events", `{"desktop_id":"","text":""}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected an unusable event to be accepted, got status %d", rec.Code)
	}

	if got := len(readThread(t, s, "research-box").Steps); got != 0 {
		t.Fatalf("expected nothing to be recorded, got %d steps", got)
	}
}

func TestClearDiscardsTranscript(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"hello"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/desktops/chat/clear",
		strings.NewReader("id=research-box"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	s.clearChatHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("clearChatHandler: got status %d", rec.Code)
	}

	thread := readThread(t, s, "research-box")

	if len(thread.Steps) != 0 || thread.Running {
		t.Fatalf("expected a cleared thread, got %+v", thread)
	}
}

// The stream sends a snapshot the moment a client connects, so a chat opened
// mid-task shows the work already in progress rather than waiting for the next
// change to arrive.
func TestStreamSendsSnapshotOnConnect(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"$ whoami"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/desktops/chat/stream?id=research-box", nil)
	rec := httptest.NewRecorder()

	s.chatStreamHandler(rec, req.WithContext(ctx))

	body := rec.Body.String()

	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("expected an SSE data frame, got %q", body)
	}

	if !strings.Contains(body, "$ whoami") {
		t.Fatalf("expected the in-flight step in the first frame, got %q", body)
	}

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected an event-stream content type, got %q", got)
	}
}

func TestChatHandlerRequiresID(t *testing.T) {
	s := chatServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/desktops/chat", nil)
	rec := httptest.NewRecorder()

	s.chatHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without an id, got %d", rec.Code)
	}
}

func TestStepImageURLDeduplicatesIdenticalScreenshots(t *testing.T) {
	s := chatServer(t)

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	pngData := buf.Bytes()

	url1 := s.stepImageURL("box-1", pngData)
	if url1 == "" {
		t.Fatal("expected url1, got empty")
	}

	url2 := s.stepImageURL("box-1", pngData)
	if url2 != url1 {
		t.Fatalf("expected identical screenshot to reuse URL, got %s vs %s", url1, url2)
	}
}

// A quiet callback (a finished recording) still answers in the chat but puts
// nothing on the Query rail.
func TestQueryRailOnlyContainsActionableReplies(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want int
	}{{"final", 0}, {"message", 0}, {"query", 1}, {"approval", 1}, {"error", 1}, {"", 0}} {
		t.Run(tc.kind, func(t *testing.T) {
			s := chatServer(t)
			body, _ := json.Marshal(map[string]string{"desktop_id": "box", "kind": tc.kind,
				"content": "DECURIX TASK COMPLETED\n\nFinal reply:\nHello", "reply": "Hello"})
			postJSON(t, s.queryTextHandler, "/api/querytext", string(body))
			if got := len(s.QueryNotes.List()); got != tc.want {
				t.Fatalf("queries = %d, want %d", got, tc.want)
			}
			if entries := readThread(t, s, "box").Entries; len(entries) != 1 || entries[0].Content != "Hello" {
				t.Fatalf("reply missing from chat: %+v", entries)
			}
		})
	}
}

func TestQuietQueryTextSkipsTheQueryRail(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"research-box","vm_name":"research-box","filename":"a.txt",
		  "kind":"final","reply":"Recorded 3 steps.","quiet":true,
		  "content":"DECURIX TASK COMPLETED\n\nFinal reply:\nRecorded 3 steps."}`)

	if notes := s.QueryNotes.List(); len(notes) != 0 {
		t.Fatalf("expected no query note for a quiet callback, got %d", len(notes))
	}

	thread := readThread(t, s, "research-box")
	if len(thread.Entries) != 1 || thread.Entries[0].Content != "Recorded 3 steps." {
		t.Fatalf("expected the chat to still show the reply, got %+v", thread.Entries)
	}
}

// Answering a desktop's query - in the chat, or anywhere else a request reaches
// it as the reply - resolves that query's note out of the open list and into
// the history. Another desktop's unanswered query remains open.
func TestReplyResolvesTheQueryNote(t *testing.T) {
	s := chatServer(t)

	route := deliver(t, s, "claude-box", "pay invoice INV-1042")

	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"claude-box","task_id":"`+route.TaskID+`","content":"DECURIX QUERY REQUEST",
		  "kind":"approval","reply":"Approve paying INV-1042?"}`)
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"other-box","content":"DECURIX QUERY REQUEST","kind":"query","reply":"Which file?"}`)

	if open := s.QueryNotes.List(); len(open) != 2 {
		t.Fatalf("expected both notes open before any reply, got %d", len(open))
	}

	if reply := deliver(t, s, "claude-box", "yes"); reply.Kind != tasks.RouteReply {
		t.Fatalf("the reply routed as %+v", reply)
	}

	open := s.QueryNotes.List()
	if len(open) != 1 || open[0].DesktopID != "other-box" {
		t.Fatalf("expected only the other desktop's query to stay open, got %+v", open)
	}

	resolved := s.QueryNotes.ListResolved()
	if len(resolved) != 1 || resolved[0].DesktopID != "claude-box" {
		t.Fatalf("expected the answered query in the history, got %+v", resolved)
	}

	if resolved[0].Resolution != querynotes.ResolutionAnswered || resolved[0].ResolvedAt == nil {
		t.Fatalf("expected it resolved as answered, with a time, got %+v", resolved[0])
	}
}

// A person resolving a note by hand moves it to the history too, instead of
// deleting it.
func TestManualResolveKeepsTheNoteInTheHistory(t *testing.T) {
	s := chatServer(t)

	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"research-box","content":"DECURIX QUERY REQUEST","kind":"query","reply":"Which account?"}`)

	id := s.QueryNotes.List()[0].ID

	postForm(t, s.closeQueryNoteHandler, "/api/querynotes/close", "id="+id)

	if open := s.QueryNotes.List(); len(open) != 0 {
		t.Fatalf("expected nothing open, got %d", len(open))
	}

	resolved := s.QueryNotes.ListResolved()
	if len(resolved) != 1 || resolved[0].ID != id || resolved[0].Resolution != querynotes.ResolutionManual {
		t.Fatalf("expected the note in the history as manual, got %+v", resolved)
	}
}

// A finished task leaves no query note, so the pages learn that the work is done from the finished feed: a page's
// first look gets only where "now" is, and later looks get each task finished since, named by its request.
func TestAFinishedTaskIsInTheFinishedFeed(t *testing.T) {
	s := chatServer(t)

	finished := func(query string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		s.finishedTasksHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/finished"+query, nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%v: %s", err, rec.Body.String())
		}
		return out
	}

	first := finished("")
	if items := first["items"].([]any); len(items) != 0 || first["latest"].(float64) != 0 {
		t.Fatalf("a first look must not replay anything: %v", first)
	}

	deliver(t, s, "research-box", "count the files in /etc")
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"research-box","vm_name":"Research","filename":"a.txt","kind":"final","reply":"241 files.",
		  "content":"DECURIX TASK COMPLETED\n\nFinal reply:\n241 files."}`)
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"research-box","vm_name":"Research","filename":"b.txt","kind":"query","reply":"Which folder?",
		  "content":"DECURIX QUERY REQUEST\n\nWhich folder?"}`)

	got := finished("?after=0")
	items := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want only the finished task (a question is a query note), got %v", items)
	}
	item := items[0].(map[string]any)
	if item["vm_name"] != "Research" || item["message"] != "241 files." || item["task"] != "count the files in /etc" {
		t.Fatalf("finished task reads %v", item)
	}
	if again := finished("?after=" + strconv.Itoa(int(got["latest"].(float64)))); len(again["items"].([]any)) != 0 {
		t.Fatalf("asking after the latest gave %v", again)
	}
}
