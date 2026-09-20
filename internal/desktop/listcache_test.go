package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// listCounter is a stand-in Docker daemon that answers ContainerList with no
// containers, slowly, and counts how many times it was asked.
func listCounter(t *testing.T, delay time.Duration) (*Manager, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/containers/json") {
			calls.Add(1)
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)

	cli, err := client.NewClientWithOpts(
		client.WithHTTPClient(ts.Client()),
		client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")),
		client.WithVersion("1.43"),
	)
	if err != nil {
		t.Fatal(err)
	}

	return NewManager(cli), &calls
}

func TestListCollapsesConcurrentCallersIntoOneDaemonCall(t *testing.T) {
	m, calls := listCounter(t, 100*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.List(t.Context()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("30 concurrent List calls made %d daemon calls, want 1", got)
	}
}

func TestListIsReusedInsideTheTTLAndRefreshedAfterIt(t *testing.T) {
	m, calls := listCounter(t, 0)

	for i := 0; i < 5; i++ {
		if _, err := m.List(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("5 sequential Lists inside the TTL made %d daemon calls, want 1", got)
	}

	time.Sleep(listCacheTTL + 50*time.Millisecond)

	if _, err := m.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("a List after the TTL should refresh: %d daemon calls, want 2", got)
	}
}

func TestListSeesTheStateAfterALifecycleChange(t *testing.T) {
	m, calls := listCounter(t, 0)

	if _, err := m.List(t.Context()); err != nil {
		t.Fatal(err)
	}

	m.invalidateList()

	if _, err := m.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("a List right after invalidateList must not be served from cache: %d calls, want 2", got)
	}
}

func TestListResultComputedBeforeAChangeIsNotCachedOverIt(t *testing.T) {
	m, calls := listCounter(t, 150*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.List(t.Context())
	}()

	time.Sleep(50 * time.Millisecond)
	m.invalidateList() // a lifecycle change lands while the list is in flight
	<-done

	if _, err := m.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("the in-flight result predates the change and must not be reused: %d calls, want 2", got)
	}
}

func TestAWaitingListCallerCanHangUpWithoutFailingTheOthers(t *testing.T) {
	m, _ := listCounter(t, 200*time.Millisecond)

	leaderDone := make(chan error, 1)
	go func() {
		_, err := m.List(t.Context())
		leaderDone <- err
	}()
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := contextWithCancel(t)
	cancel()
	if _, err := m.List(ctx); err == nil {
		t.Fatal("a caller whose context is already cancelled should get its own error")
	}

	if err := <-leaderDone; err != nil {
		t.Fatalf("the leader must not be failed by a follower hanging up: %v", err)
	}
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(t.Context())
}
