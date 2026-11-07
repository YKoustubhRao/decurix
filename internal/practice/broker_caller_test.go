package practice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// testAddr is where httptest.NewRequest says a request comes from.
var testAddr = netip.MustParseAddr("192.0.2.1")

// desktopAt is a Caller that knows one practice desktop, id, at httptest's address.
func desktopAt(id string) func(context.Context, netip.Addr) (string, bool) {
	return func(_ context.Context, a netip.Addr) (string, bool) { return id, a == testAddr }
}

// runDesktopAt is a Caller that says httptest's address is b's open run's desktop, whichever run that is.
func runDesktopAt(b *Broker) func(context.Context, netip.Addr) (string, bool) {
	return func(_ context.Context, a netip.Addr) (string, bool) {
		run := b.active()
		if run == nil || a != testAddr {
			return "", false
		}
		return run.desktopID, true
	}
}

// C-25: the broker knows the practice desktop by the address a callback comes from, never by the desktop_id in its
// body - the mock app shares the practice network and could name the desktop.
func TestBrokerKnowsTheDesktopByItsAddress(t *testing.T) {
	desk, mock := netip.MustParseAddr("172.30.0.5"), netip.MustParseAddr("172.30.0.6")
	var asked atomic.Int32
	b := &Broker{LLM: &fakeChat{usage: 1}, Caller: func(_ context.Context, a netip.Addr) (string, bool) {
		asked.Add(1)
		switch a {
		case desk:
			return "practice-r1", true
		case mock:
			return "", false // the mock is no practice desktop
		}
		return "", false
	}}
	b.Begin("practice-r1", 100)
	obs := b.StartCase("t1")
	post := func(from netip.Addr, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/querytext", strings.NewReader(body))
		r.RemoteAddr = netip.AddrPortFrom(from, 40000).String()
		b.Handler().ServeHTTP(w, r)
		return w.Code
	}
	final := `{"desktop_id":"practice-r1","kind":"final","reply":"done"}`

	// The mock naming the practice desktop is refused, and records nothing.
	if code := post(mock, final); code != http.StatusForbidden {
		t.Fatalf("a callback from the mock naming the desktop = %d, want 403", code)
	}
	select {
	case o := <-obs:
		t.Fatalf("the mock's callback was recorded: %+v", o)
	default:
	}
	// Asked again within the window, the mock's address is not asked about again.
	before := asked.Load()
	post(mock, final)
	if asked.Load() != before {
		t.Fatal("a known non-desktop address made the broker ask Docker again at once")
	}

	// The desktop at its own address, naming another desktop, is refused.
	if code := post(desk, `{"desktop_id":"worker","kind":"final","reply":"x"}`); code != http.StatusForbidden {
		t.Fatalf("a body naming another desktop = %d, want 403", code)
	}
	// The desktop at its own address is accepted, naming itself or naming nothing.
	if code := post(desk, final); code != http.StatusOK {
		t.Fatalf("the desktop's own callback = %d", code)
	}
	if code := post(desk, `{"kind":"query","reply":"which one?"}`); code != http.StatusOK {
		t.Fatalf("the desktop's callback with no desktop_id = %d", code)
	}
	if o := <-obs; o.Text != "done" {
		t.Fatalf("observation = %+v", o)
	}

	// With no Caller at all, nothing is accepted (fail closed).
	closed := &Broker{LLM: &fakeChat{usage: 1}}
	closed.Begin("practice-r1", 100)
	w := httptest.NewRecorder()
	closed.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/querytext", strings.NewReader(final)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("a broker with no caller map accepted a callback: %d", w.Code)
	}
}
