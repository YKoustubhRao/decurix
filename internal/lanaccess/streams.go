package lanaccess

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
)

// A paired device's request passes the gate once, but what it opens can stay open for hours: a desktop's screen over
// a WebSocket, an event stream. Unpairing the device must end those too, not only its next request (review RW1-B-04),
// so every request a device token lets through is tracked under that device until it ends, and CloseDevice ends them.

// DeviceIdentifier is the optional part of a DeviceRegistry that names the device a token belongs to.
// *devices.Store satisfies it.
type DeviceIdentifier interface {
	DeviceID(token string) (id string, ok bool)
}

// streams are the live requests of each paired device: a cancel for each one's context, and the connection a
// WebSocket hijacked from it.
type streams struct {
	mu   sync.Mutex
	next int
	open map[string]map[int]*stream
}

type stream struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	conn   net.Conn
}

func (s *stream) setConn(c net.Conn) {
	s.mu.Lock()
	s.conn = c
	s.mu.Unlock()
}

func (s *stream) close() {
	s.cancel()
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.mu.Unlock()
}

// track registers one live request of device id, and returns its context and the function that forgets it.
func (t *streams) track(ctx context.Context, id string) (context.Context, *stream, func()) {
	ctx, cancel := context.WithCancel(ctx)
	st := &stream{cancel: cancel}
	t.mu.Lock()
	if t.open == nil {
		t.open = map[string]map[int]*stream{}
	}
	if t.open[id] == nil {
		t.open[id] = map[int]*stream{}
	}
	t.next++
	key := t.next
	t.open[id][key] = st
	t.mu.Unlock()
	return ctx, st, func() {
		t.mu.Lock()
		delete(t.open[id], key)
		if len(t.open[id]) == 0 {
			delete(t.open, id)
		}
		t.mu.Unlock()
		cancel()
	}
}

// closeDevice ends every live request of device id, and says how many it ended.
func (t *streams) closeDevice(id string) int {
	t.mu.Lock()
	open := t.open[id]
	delete(t.open, id)
	t.mu.Unlock()
	for _, st := range open {
		st.close()
	}
	return len(open)
}

// trackingWriter is a ResponseWriter whose hijacked connection is recorded on its stream, so it can be closed.
type trackingWriter struct {
	http.ResponseWriter
	st *stream
}

func (w trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("lanaccess: the connection cannot be taken over")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		w.st.setConn(conn)
	}
	return conn, rw, err
}

func (w trackingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// CloseDevice ends every request device id still has open through the gate - a screen's WebSocket, an event stream
// - and says how many it ended. Registered on the devices store's OnRevoke.
func (s *Store) CloseDevice(id string) int { return s.streams.closeDevice(id) }

// serveDevice serves a request a device token let through, tracked under that device while it lasts.
func (s *Store) serveDevice(next http.Handler, w http.ResponseWriter, r *http.Request) {
	ider, ok := s.deviceRegistry().(DeviceIdentifier)
	cookie, err := r.Cookie(deviceCookie)
	if !ok || err != nil {
		next.ServeHTTP(w, r)
		return
	}
	id, found := ider.DeviceID(cookie.Value)
	if !found {
		next.ServeHTTP(w, r)
		return
	}
	ctx, st, done := s.streams.track(r.Context(), id)
	defer done()
	next.ServeHTTP(trackingWriter{ResponseWriter: w, st: st}, r.WithContext(ctx))
}
