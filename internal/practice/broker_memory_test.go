package practice

// broker_memory_test.go: a memory note the run's practice desktop sends is written into that desktop's own memory,
// so the run's next case reads it (a lesson from case N reaches case N+1); one from any other address is dropped.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestAMemoryNoteFromTheRunsDesktopIsKeptOnThatDesktop(t *testing.T) {
	b := &Broker{LLM: &fakeChat{usage: 1}}
	b.Caller = runDesktopAt(b)
	type kept struct{ desktop, file, content string }
	var got []kept
	b.Remember = func(_ context.Context, desktopID, filename, content string) error {
		got = append(got, kept{desktopID, filename, content})
		return nil
	}
	b.Begin("practice-r1", 100)
	post := func(from netip.Addr, form url.Values) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/desktops/memory", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = netip.AddrPortFrom(from, 40000).String()
		b.Handler().ServeHTTP(w, r)
		return w.Code
	}
	note := url.Values{"desktop_id": {"anything"}, "title": {"Press Search after the quarter"},
		"summary": {"The month list follows the quarter only after Search."}, "category": {"lesson"}}
	if code := post(testAddr, note); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(got) != 1 || got[0].desktop != "practice-r1" || !strings.HasPrefix(got[0].file, "memory-") ||
		!strings.HasSuffix(got[0].file, ".txt") || !strings.Contains(got[0].content, "Category: lesson") ||
		!strings.Contains(got[0].content, "The month list follows the quarter only after Search.") {
		t.Fatalf("kept %+v", got)
	}
	// Another address on the practice network (the mock app) writes nothing, whatever desktop_id it names.
	if code := post(netip.MustParseAddr("192.0.2.9"), note); code != http.StatusOK || len(got) != 1 {
		t.Fatalf("a note from another address was kept: %d %+v", code, got)
	}
	// An empty note is no note.
	if post(testAddr, url.Values{"title": {" "}}); len(got) != 1 {
		t.Fatalf("an empty note was kept: %+v", got)
	}
}
