// Package httpapi: screenholders_test.go checks that a desktop's screen opens in one window at a time.
package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A second window is refused while the first holds the screen, and gets it once the first lets go.
func TestAScreenOpensInOneWindowAtATime(t *testing.T) {
	var h screenHolders

	releaseA, ok := h.take("box", "window-a", func() {})
	if !ok {
		t.Fatal("the first window was refused a free screen")
	}
	if _, ok := h.take("box", "window-b", func() {}); ok {
		t.Fatal("a second window opened a screen another window holds")
	}
	if _, ok := h.take("other", "window-b", func() {}); !ok {
		t.Fatal("holding one desktop's screen kept another desktop's from opening")
	}
	if held, _ := h.heldElsewhere("box", "window-b"); !held {
		t.Fatal("the second window was not told the screen is held")
	}
	if held, _ := h.heldElsewhere("box", "window-a"); held {
		t.Fatal("the holding window was told its own screen is held elsewhere")
	}

	releaseA()
	if held, _ := h.heldElsewhere("box", "window-b"); held {
		t.Fatal("the screen stayed held after its window closed")
	}
	if _, ok := h.take("box", "window-b", func() {}); !ok {
		t.Fatal("the second window was refused after the first closed")
	}
}

// The same window reconnecting takes the screen back and its older stream is closed; that older stream ending
// afterwards does not free the screen from under the new one.
func TestTheSameWindowReconnectingTakesItsScreenBack(t *testing.T) {
	var h screenHolders

	kicked := make(chan struct{})
	releaseOld, ok := h.take("box", "window-a", func() { close(kicked) })
	if !ok {
		t.Fatal("refused a free screen")
	}
	if _, ok := h.take("box", "window-a", func() {}); !ok {
		t.Fatal("the same window was refused its own screen on reconnecting")
	}
	select {
	case <-kicked:
	case <-time.After(time.Second):
		t.Fatal("the window's older stream was not closed")
	}

	releaseOld()
	if _, ok := h.take("box", "window-b", func() {}); ok {
		t.Fatal("the older stream ending freed the screen the newer one holds")
	}
}

// A stream that names no usable window is a window of its own: it never matches another's token.
func TestAStreamWithoutAWindowIsAWindowOfItsOwn(t *testing.T) {
	for _, query := range []string{"", "?holder=", "?holder=" + strings.Repeat("a", 65), "?holder=a%20b", "?holder=..%2F"} {
		r := httptest.NewRequest(http.MethodGet, "/desktop/box/websockify"+query, nil)
		first, second := screenHolderToken(r), screenHolderToken(r)
		if !strings.HasPrefix(first, "anon-") || first == second {
			t.Errorf("%q: tokens %q and %q, want two fresh anonymous ones", query, first, second)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/desktop/box/websockify?holder=3f9a_b-01", nil)
	if got := screenHolderToken(r); got != "3f9a_b-01" {
		t.Errorf("a usable window token came back as %q", got)
	}
}

// The screen page asks whether another window holds the screen, on either listener.
func TestTheScreenPageCanAskWhetherTheScreenIsHeld(t *testing.T) {
	s := &Server{}
	primary, gated := listenerMuxes(s)
	if _, ok := s.screens.take("box", "window-a", func() {}); !ok {
		t.Fatal("refused a free screen")
	}

	ask := func(mux *http.ServeMux, query string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/screen-holder"+query, nil))
		var answer map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer
	}

	for _, mux := range []*http.ServeMux{primary, gated} {
		if code, answer := ask(mux, "?id=box&holder=window-b"); code != http.StatusOK || answer["held"] != true || answer["since"] == nil {
			t.Errorf("another window: %d %v", code, answer)
		}
		if code, answer := ask(mux, "?id=BOX&holder=window-a"); code != http.StatusOK || answer["held"] != false {
			t.Errorf("the holding window itself: %d %v", code, answer)
		}
		if code, answer := ask(mux, "?id=free&holder=window-b"); code != http.StatusOK || answer["held"] != false {
			t.Errorf("a free screen: %d %v", code, answer)
		}
		if code, _ := ask(mux, ""); code != http.StatusBadRequest {
			t.Errorf("no desktop named: %d", code)
		}
	}
}
