// privacy_purge_test.go checks POST /api/privacy/purge: the owner at this machine purges a value and gets counts;
// a short value is refused; the gated listener has no such route; a desktop is refused; and the value never shows in
// the log, the answer, or the judgement that records the purge.

package httpapi

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/purge"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

const purgeLeak = "copperwillowharbour"

// purgeServer is a Server with the stores a purge covers, the desktops' network known, and a note holding the value.
func purgeServer(t *testing.T) *Server {
	t.Helper()
	s := fullServer(t)
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1")
	s.Access.Middleware.Gated = isGated // as main.go sets it: the primary listener's actor is the local owner
	db := testDB(t)
	s.DB = db
	var err error
	if s.Tasks, err = tasks.NewStore(db); err != nil {
		t.Fatal(err)
	}
	if s.QueryNotes, err = querynotes.NewStore(db); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.LLMRec, err = llmrec.NewStore(db, blobs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO query_notes (id, desktop_id, vm_name, filename, content, created_ns)
		VALUES ('q1', 'd1', 'vm', 'q.txt', ?, 1)`, "is "+purgeLeak+" the code?"); err != nil {
		t.Fatal(err)
	}
	return s
}

// purgePost posts form to the route on mux from remote.
func purgePost(mux http.Handler, remote string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/privacy/purge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// captureLog sends the standard logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// The owner at this machine purges the value: the answer is the counts and the judgement, and the value is in no
// log line, no answer, no judgement, and no longer in the note.
func TestPrivacyPurgeByTheOwnerAnswersCountsAndNeverTheValue(t *testing.T) {
	s := purgeServer(t)
	logs := captureLog(t)
	primary, _ := listenerMuxes(s)
	rec := purgePost(primary, fromHost, url.Values{"value": {" " + purgeLeak + "\n"}, "reason": {"pasted into a query"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var res purge.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Counts.QueryNotes != 1 || res.Judgement == "" {
		t.Errorf("result = %+v", res)
	}
	var content string
	_ = s.DB.QueryRow(`SELECT content FROM query_notes WHERE id = 'q1'`).Scan(&content)
	if strings.Contains(content, purgeLeak) || !strings.Contains(content, purge.Mark) {
		t.Errorf("the note is %q", content)
	}
	j, ok, err := s.Tasks.Judgement(res.Judgement)
	if err != nil || !ok || j.Actor.ID != tasks.LocalOwner.ID || j.Kind != tasks.JudgementPurge || j.Door != purgeDoor {
		t.Fatalf("judgement = %+v %v %v", j, ok, err)
	}
	jb, _ := json.Marshal(j)
	for name, b := range map[string][]byte{"log": logs.Bytes(), "answer": rec.Body.Bytes(), "judgement": jb} {
		if bytes.Contains(b, []byte(purgeLeak)) {
			t.Errorf("the value is in the %s: %s", name, b)
		}
	}
}

// A value of five characters is refused, in words that do not repeat it, and nothing changes.
func TestPrivacyPurgeRefusesAShortValue(t *testing.T) {
	s := purgeServer(t)
	primary, _ := listenerMuxes(s)
	rec := purgePost(primary, fromHost, url.Values{"value": {"abcde"}, "reason": {"leaked"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "6 characters") ||
		strings.Contains(rec.Body.String(), "abcde") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM judgements`).Scan(&n)
	if n != 0 {
		t.Errorf("a refused purge recorded %d judgements", n)
	}
}

// The gated listener has no purge route, and the handler refuses a gated request should it ever be mounted there; a
// desktop's address on the primary listener is refused before the handler runs.
func TestPrivacyPurgeIsNotForTheGatedListenerOrADesktop(t *testing.T) {
	s := purgeServer(t)
	primary, gated := listenerMuxes(s)
	form := url.Values{"value": {purgeLeak}, "reason": {"leaked"}}

	if _, pattern := gated.Handler(httptest.NewRequest(http.MethodPost, "/api/privacy/purge", nil)); pattern == "/api/privacy/purge" {
		t.Error("the gated listener serves the purge")
	}
	if rec := purgePost(MarkGated(gated), fromHost, form); rec.Code == http.StatusOK {
		t.Errorf("through the gated listener: %d", rec.Code)
	}
	if rec := purgePost(MarkGated(http.HandlerFunc(s.privacyPurgeHandler)), fromHost, form); rec.Code != http.StatusForbidden {
		t.Errorf("the handler, gated: %d, want 403", rec.Code)
	}
	if rec := purgePost(primary, "172.30.0.5:40000", form); rec.Code != http.StatusForbidden {
		t.Errorf("from a desktop: %d, want 403", rec.Code)
	}
	var content string
	_ = s.DB.QueryRow(`SELECT content FROM query_notes WHERE id = 'q1'`).Scan(&content)
	if !strings.Contains(content, purgeLeak) {
		t.Error("a refused caller purged the note")
	}
}
