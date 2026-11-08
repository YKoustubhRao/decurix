package practice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// The scripted expert's replies are the person's words on a practice desktop: the runner keeps them (KeepReply), the
// desktop reads them back from its own address as the control plane serves a person's judgements - by task, or every
// task of the worker newest first - and a reset forgets them. Nothing the desktop sends makes one; another address
// reads nothing.
func TestTheBrokerServesTheExpertsRepliesAsThePersonsWords(t *testing.T) {
	b := &Broker{LLM: &fakeChat{usage: 1}}
	b.Caller = runDesktopAt(b)
	if id := b.KeepReply("t1", "Which code?", "PX-7"); id != "" {
		t.Fatalf("a reply kept with no run open: %q", id)
	}
	b.Begin("practice-r1", 100)
	get := func(from netip.Addr, method, target string) (int, []map[string]any) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, target, nil)
		r.RemoteAddr = netip.AddrPortFrom(from, 40000).String()
		b.Handler().ServeHTTP(w, r)
		var got struct{ Judgements []map[string]any }
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got.Judgements
	}
	task1, task2 := "t0000000000000001", "t0000000000000002"
	first := b.KeepReply(task1, "Which project code does this bill take?", "From now on, print shops go to PX-7.")
	second := b.KeepReply(task2, "Which terms?", "Net  45")
	if !strings.HasPrefix(first, "j_") || second == "" || first == second {
		t.Fatalf("ids %q %q", first, second)
	}

	code, rows := get(testAddr, http.MethodGet, "/api/desktops/judgements?scope=worker&kind=answer")
	if code != http.StatusOK || len(rows) != 2 || rows[0]["id"] != second || rows[1]["id"] != first {
		t.Fatalf("worker scope: %d %+v", code, rows)
	}
	actor, _ := rows[0]["actor"].(map[string]any)
	sum := sha256.Sum256([]byte("Net 45")) // (the text's spacing collapsed, as the agent hashes a kept reply)
	if actor["kind"] != tasks.ActorScripted || rows[0]["kind"] != tasks.JudgementAnswer || rows[0]["text"] != "Net  45" ||
		rows[0]["text_sha256"] != hex.EncodeToString(sum[:]) ||
		rows[1]["asked"] != "Which project code does this bill take?" || rows[0]["derived_from"] != "" || rows[0]["secret"] != false {
		t.Fatalf("a reply as served: %+v", rows[0])
	}
	if code, rows := get(testAddr, http.MethodGet, "/api/desktops/judgements?task="+task1); code != http.StatusOK || len(rows) != 1 || rows[0]["id"] != first {
		t.Fatalf("by task: %d %+v", code, rows)
	}
	if code, rows := get(testAddr, http.MethodGet, "/api/desktops/judgements?scope=worker&limit=1"); code != http.StatusOK || len(rows) != 1 || rows[0]["id"] != second {
		t.Fatalf("limit: %d %+v", code, rows)
	}
	if code, rows := get(testAddr, http.MethodGet, "/api/desktops/judgements?skill=enter-bill"); code != http.StatusOK || len(rows) != 0 {
		t.Fatalf("a skill's words: %d %+v", code, rows)
	}
	for name, target := range map[string]string{
		"nothing named":  "/api/desktops/judgements",
		"another scope":  "/api/desktops/judgements?scope=org",
		"another kind":   "/api/desktops/judgements?scope=worker&kind=verdict",
		"a bad task":     "/api/desktops/judgements?task=../x",
		"a bad limit":    "/api/desktops/judgements?scope=worker&limit=900",
		"scope and task": "/api/desktops/judgements?scope=worker&task=" + task1,
	} {
		if code, _ := get(testAddr, http.MethodGet, target); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := get(netip.MustParseAddr("192.0.2.9"), http.MethodGet, "/api/desktops/judgements?scope=worker"); code != http.StatusForbidden {
		t.Fatalf("another address: %d", code)
	}
	if code, _ := get(testAddr, http.MethodPost, "/api/desktops/judgements?scope=worker"); code != http.StatusMethodNotAllowed {
		t.Fatalf("a post: %d", code)
	}

	b.ForgetReplies()
	if code, rows := get(testAddr, http.MethodGet, "/api/desktops/judgements?scope=worker"); code != http.StatusOK || len(rows) != 0 {
		t.Fatalf("after a reset: %d %+v", code, rows)
	}
	b.End()
	if code, _ := get(testAddr, http.MethodGet, "/api/desktops/judgements?scope=worker"); code != http.StatusNotFound {
		t.Fatalf("with no run: %d", code)
	}
}
