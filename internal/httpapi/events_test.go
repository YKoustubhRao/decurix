// Tests for the dashboard's event stream (events.go): a chat change reaches an open stream as its desktop's id, the
// stream is on both listeners like the polls it speeds up, and a desktop is refused it.
package httpapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventStreamSaysWhichDesktopChanged(t *testing.T) {
	s := chatServer(t)
	server := httptest.NewServer(http.HandlerFunc(s.eventsHandler))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}

	lines := bufio.NewScanner(resp.Body)
	read := func(want string) {
		t.Helper()
		for lines.Scan() {
			if strings.Contains(lines.Text(), want) {
				return
			}
		}
		t.Fatalf("the stream ended before %q: %v", want, lines.Err())
	}
	read("event: hello")

	deliver(t, s, "box-a", "file the report")
	read(`"desktops":["box-a"]`)
}

func TestEventStreamIsOnBothListenersAndRefusedToADesktop(t *testing.T) {
	primary, gated := listenerMuxes(fullServer(t))
	for name, mux := range map[string]*http.ServeMux{"primary": primary, "gated": gated} {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/api/events", nil)); pattern != "/api/events" {
			t.Errorf("/api/events on the %s listener: served as %q", name, pattern)
		}
	}
	if containerMayCall(http.MethodGet, "/api/events") {
		t.Error("a desktop may open the dashboard's event stream")
	}
}

func TestEventPayload(t *testing.T) {
	if got := string(eventPayload(map[string]bool{"b": true, "a": true})); got != `{"desktops":["a","b"]}` {
		t.Errorf("payload %s", got)
	}
	if got := string(eventPayload(map[string]bool{"a": true, "": true})); got != `{"all":true}` {
		t.Errorf("payload %s", got)
	}
}
