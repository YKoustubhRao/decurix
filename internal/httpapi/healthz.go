// healthz.go is the control plane's lifecycle as its supervisors see it: GET /healthz says which binary answered
// (version, pid, start time, instance) and whether it is draining, and Lifecycle is the graceful drain at shutdown.
// Every listener is wrapped by Lifecycle.Wrap; when the process is told to stop (an app update recreates the control
// plane's container), Drain refuses new requests with 503 and Retry-After, ends the long-lived streams at once (event
// streams, WebSockets: the screen, host and phone apps, phone links), and lets every other request in flight (a model
// call through /api/llm/, a desktop being created) finish within the drain budget (DECURIX_SHUTDOWN_DRAIN, default
// 90 s). The listeners stay open while it runs, so the app reads draining:true and does not count the swap as a
// death, and a desktop's model call is told to come back (a 503 it retries) rather than refused. docs/running.md.
package httpapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// healthzPath is the health route, primary listener only (RegisterLocalRoutes); a drain still answers it.
const healthzPath = "/healthz"

// healthService names what answers /healthz, so a supervisor knows it is a Decurix control plane and not another
// program on the port.
const healthService = "decurix-control-plane"

// DefaultDrainBudget is how long a shutdown lets requests in flight finish, unless DECURIX_SHUTDOWN_DRAIN says
// otherwise. The compose files give the container 100 s to stop (stop_grace_period), so the drain ends first.
const DefaultDrainBudget = 90 * time.Second

// maxRetryAfter caps the Retry-After a refused request is given: a desktop's model client waits that long, up to
// three times, before it gives up (agent/thebot internal/llm/retry.go), then retries a refused connection for about a
// minute more. Thirty seconds spans a full drain and the new control plane's start.
const maxRetryAfter = 30

// Lifecycle is this process's lifecycle as the listeners and /healthz see it. main sets Version and Instance before
// the listeners start, wraps each listener's handler with Wrap and gives it BaseContext, and calls Drain then Abort
// at shutdown.
type Lifecycle struct {
	Version  string
	Instance string
	Started  time.Time

	mu       sync.Mutex
	draining bool
	deadline time.Time
	active   int
	idle     chan struct{}

	streams    context.Context
	endStreams context.CancelFunc
	base       context.Context
	abort      context.CancelFunc
}

// NewLifecycle returns a Lifecycle for a process that started now.
func NewLifecycle(version string) *Lifecycle {
	streams, endStreams := context.WithCancel(context.Background())
	base, abort := context.WithCancel(context.Background())
	return &Lifecycle{Version: version, Started: time.Now(), streams: streams, endStreams: endStreams, base: base, abort: abort}
}

// Life is this process's Lifecycle: the one /healthz reports and cmd/decurix/main.go drains.
var Life = NewLifecycle("dev")

// healthzHandler answers GET /healthz from Life (Lifecycle.ServeHealth).
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	Life.ServeHealth(w, r)
}

// healthReport is the body of GET /healthz.
type healthReport struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	Instance  string `json:"instance,omitempty"`
	Draining  bool   `json:"draining"`
}

// ServeHealth answers which binary this is and whether it is draining: no database, no Docker, nothing that can be
// slow, so a supervisor's short timeout never mistakes a busy control plane for a dead one.
func (l *Lifecycle) ServeHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		jsonStatusError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, healthReport{
		Service:   healthService,
		Version:   l.Version,
		PID:       os.Getpid(),
		StartedAt: l.Started.UTC().Format(time.RFC3339),
		Instance:  l.Instance,
		Draining:  l.Draining(),
	})
}

// Draining reports whether a drain has started.
func (l *Lifecycle) Draining() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.draining
}

// BaseContext is every request's base context (http.Server.BaseContext): Abort ends it, so a request still running
// when the drain budget is spent is told to stop before the database closes under it.
func (l *Lifecycle) BaseContext(net.Listener) context.Context {
	return l.base
}

// Wrap is the outermost handler of a listener. It counts the requests in flight for Drain, refuses new ones with 503
// and Retry-After once a drain has started (GET /healthz still answers, saying so), and gives each long-lived stream a
// context that ends the moment the drain starts, so a stream never holds the drain open.
func (l *Lifecycle) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := longLived(r)
		l.mu.Lock()
		if l.draining && r.URL.Path != healthzPath {
			retry := l.retryAfterLocked(time.Now())
			l.mu.Unlock()
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			w.Header().Set("Connection", "close")
			jsonStatusError(w, http.StatusServiceUnavailable, "Decurix is restarting. Try again in a moment.")
			return
		}
		if !stream {
			l.active++
		}
		l.mu.Unlock()

		if stream {
			ctx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(l.streams, cancel)
			defer stop()
			defer cancel()
			r = r.WithContext(ctx)
		} else {
			defer l.finished()
		}
		next.ServeHTTP(w, r)
	})
}

// finished counts one request in flight as done, and wakes a Drain waiting for the last one.
func (l *Lifecycle) finished() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.active == 0 && l.idle != nil {
		close(l.idle)
		l.idle = nil
	}
}

// Drain starts the drain: from now on new requests are refused (503, Retry-After), the long-lived streams are told to
// end, and it waits until every other request in flight has finished or ctx ends. It returns how many were still
// running (0: all finished). Calling it again waits again.
func (l *Lifecycle) Drain(ctx context.Context) int {
	l.mu.Lock()
	if !l.draining {
		l.draining = true
		if deadline, ok := ctx.Deadline(); ok {
			l.deadline = deadline
		}
	}
	l.endStreams()
	if l.active == 0 {
		l.mu.Unlock()
		return 0
	}
	if l.idle == nil {
		l.idle = make(chan struct{})
	}
	idle := l.idle
	l.mu.Unlock()

	select {
	case <-idle:
		return 0
	case <-ctx.Done():
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.active
	}
}

// Abort ends every request's context (BaseContext): what a drain that ran out of budget does before the listeners
// are closed.
func (l *Lifecycle) Abort() {
	l.endStreams()
	l.abort()
}

// retryAfterLocked is the Retry-After for a request refused during the drain, in seconds: what is left of the drain,
// between 1 and maxRetryAfter. Called with l.mu held.
func (l *Lifecycle) retryAfterLocked(now time.Time) int {
	if l.deadline.IsZero() {
		return 5
	}
	left := int(l.deadline.Sub(now).Seconds() + 0.999)
	return min(max(left, 1), maxRetryAfter)
}

// longLived reports whether r opens a stream that lasts as long as its client wants: a WebSocket or other upgrade
// (the screen, host and phone apps, a phone's link), or a server-sent event stream (EventSource always asks for
// text/event-stream; the chat's stream is also known by its path). These are ended at once by a drain, never waited
// for; their clients reconnect to the control plane that comes next.
func longLived(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		return true
	}
	return strings.HasSuffix(r.URL.Path, "/stream")
}

// DrainBudget reads DECURIX_SHUTDOWN_DRAIN's value: a Go duration ("90s", "2m") or a number of seconds. Empty is
// DefaultDrainBudget; "0" drains nothing (streams still end, the listeners close at once). Anything else is the
// default, with a note for the log saying why.
func DrainBudget(raw string) (time.Duration, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultDrainBudget, ""
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, ""
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d, ""
	}
	return DefaultDrainBudget, "DECURIX_SHUTDOWN_DRAIN=" + strconv.Quote(raw) + " is not a duration or a number of seconds; using " + DefaultDrainBudget.String()
}
