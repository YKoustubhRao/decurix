// The dashboard's event stream: GET /api/events tells a page which desktops changed, the moment they change, so it
// fetches their cards then rather than on its next poll (after agent-orchestrator's change stream).
//
// An event is a hint, never state: it carries only desktop ids, and the page reads what changed through the routes
// it already polls. Nothing a desktop said travels on it, so the stream cannot leak what those routes withhold. The
// page keeps a slower poll while the stream is up and its usual one when it is not, and fetches everything again on
// every (re)connect: an event missed while disconnected is never relied on.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// eventBatch is how long changes are gathered into one event: a turn's reply, its note and its record land together.
const eventBatch = 150 * time.Millisecond

// eventHeartbeat is how often an idle stream says it is alive, so tunnels and proxies do not close it.
const eventHeartbeat = 20 * time.Second

// eventHub fans "desktop X changed" out to every open event stream. Its zero value is ready; publish never blocks.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan string]bool
	// chat starts the forwarding of the chat store's changes, once (Server.watchChanges).
	chat sync.Once
}

// publish tells every open stream that desktopID changed ("" for "anything may have").
func (h *eventHub) publish(desktopID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- desktopID:
		default:
			// A stream this far behind is told to read everything instead.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- "":
			default:
			}
		}
	}
}

// subscribe opens a stream's channel, and returns the func that closes it.
func (h *eventHub) subscribe() (<-chan string, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[chan string]bool{}
	}
	ch := make(chan string, 256)
	h.subs[ch] = true
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
	}
}

// watchChanges forwards every chat change (a request, a step, a reply, a turn's end) to the event streams, from the
// first stream on.
func (s *Server) watchChanges() {
	s.events.chat.Do(func() {
		if s.Chat == nil {
			return
		}
		changes, _ := s.Chat.Watch()
		go func() {
			for id := range changes {
				s.events.publish(id)
			}
		}()
	})
}

// eventsHandler serves GET /api/events, a text/event-stream: first a "hello" naming this process (a page fetches
// everything on every hello), then one event per batch of changes, {"desktops":[ids]} or {"all":true}, and a comment
// line every eventHeartbeat. It ends when the client goes or the control plane shuts down.
func (s *Server) eventsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonStatusError(w, http.StatusInternalServerError, "streaming is not supported here")
		return
	}
	s.watchChanges()
	changes, stop := s.events.subscribe()
	defer stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	hello, _ := json.Marshal(map[string]string{"boot": fmt.Sprint(processStarted.UnixNano())})
	fmt.Fprintf(w, "retry: 3000\nevent: hello\ndata: %s\n\n", hello)
	flusher.Flush()

	heartbeat := time.NewTicker(eventHeartbeat)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case first := <-changes:
			batch := map[string]bool{first: true}
			gather := time.NewTimer(eventBatch)
		gathering:
			for {
				select {
				case id := <-changes:
					batch[id] = true
				case <-gather.C:
					break gathering
				case <-ctx.Done():
					gather.Stop()
					return
				}
			}
			fmt.Fprintf(w, "data: %s\n\n", eventPayload(batch))
			flusher.Flush()
		}
	}
}

// eventPayload is one event's data: the desktops that changed, sorted, or {"all":true} when any change was "".
func eventPayload(batch map[string]bool) []byte {
	if batch[""] {
		return []byte(`{"all":true}`)
	}
	ids := make([]string, 0, len(batch))
	for id := range batch {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out, _ := json.Marshal(map[string][]string{"desktops": ids})
	return out
}
