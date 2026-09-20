package ollama

import (
	"context"
	"testing"

	"net/http"

	"net/http/httptest"

	"sync/atomic"

	"time"
)

// The control plane preloads whatever DesktopURL names, so it must agree
// with the value desktops are created with.
func TestDesktopURL(t *testing.T) {
	t.Setenv("OLLAMA_URL", "")
	if got := DesktopURL(); got != defaultURL {
		t.Errorf("DesktopURL() with no OLLAMA_URL = %q, want %q", got, defaultURL)
	}

	// A trailing slash must not make the bundled URL look like some other
	// server, or EnsureReady would skip starting the container desktops use.
	t.Setenv("OLLAMA_URL", BundledURL+"/")
	if got := DesktopURL(); got != BundledURL {
		t.Errorf("DesktopURL() = %q, want %q", got, BundledURL)
	}
}

func TestDesktopModel(t *testing.T) {
	t.Setenv("OLLAMA_MODEL", "")
	if got := DesktopModel(); got != Model {
		t.Errorf("DesktopModel() with no OLLAMA_MODEL = %q, want %q", got, Model)
	}

	t.Setenv("OLLAMA_MODEL", "llama3.2:1b")
	if got := DesktopModel(); got != "llama3.2:1b" {
		t.Errorf("DesktopModel() = %q, want llama3.2:1b", got)
	}
}

// Only host.docker.internal is ever rewritten.
func TestDialableURLLeavesOtherHostsAlone(t *testing.T) {
	for _, u := range []string{BundledURL, "http://10.0.0.5:11434", "https://ollama.example.com"} {
		if got := dialableURL(context.Background(), u); got != u {
			t.Errorf("dialableURL(%q) = %q, want it unchanged", u, got)
		}
	}
}

func TestDefaultNumThread(t *testing.T) {
	t.Setenv("OLLAMA_NUM_THREAD", "3")
	if got := DefaultNumThread(); got != 3 {
		t.Errorf("DefaultNumThread() with OLLAMA_NUM_THREAD=3 got %d, want 3", got)
	}

	t.Setenv("OLLAMA_NUM_THREAD", "")
	got := DefaultNumThread()
	if got < 1 {
		t.Errorf("DefaultNumThread() fallback = %d, want >= 1", got)
	}
}

// fakeOllama answers /api/ps with the given resident models and counts the
// /api/generate calls (preloads) it receives.
func fakeOllama(t *testing.T, ps string) (url string, loads *atomic.Int32) {
	t.Helper()
	loads = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			_, _ = w.Write([]byte(ps))
		case "/api/generate":
			loads.Add(1)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, loads
}

func resetWarm() {
	warmState.mu.Lock()
	warmState.enabled = true
	warmState.inflight = false
	warmState.last = time.Time{}
	warmState.mu.Unlock()
}

func waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		warmState.mu.Lock()
		busy := warmState.inflight
		warmState.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("warm-up never finished")
}

func TestWarmAsyncLoadsAModelThatIsNotResident(t *testing.T) {
	url, loads := fakeOllama(t, `{"models":[]}`)
	t.Setenv("OLLAMA_URL", url)
	t.Setenv("OLLAMA_MODEL", "m:1b")
	resetWarm()

	WarmAsync()
	waitIdle(t)

	if loads.Load() != 1 {
		t.Fatalf("expected one preload, got %d", loads.Load())
	}
}

func TestWarmAsyncLeavesAResidentModelStrictlyAlone(t *testing.T) {
	url, loads := fakeOllama(t, `{"models":[{"name":"m:1b","model":"m:1b","context_length":8192}]}`)
	t.Setenv("OLLAMA_URL", url)
	t.Setenv("OLLAMA_MODEL", "m:1b")
	resetWarm()

	WarmAsync()
	waitIdle(t)

	// Not even a one-token request: it would replace the agents' cached
	// prompt prefix, and the next real request would pay ~200 s to rebuild it.
	if loads.Load() != 0 {
		t.Fatalf("a resident model was sent %d generate requests, want none", loads.Load())
	}
}

func TestWarmAsyncReloadsAModelResidentAtAnotherContextSize(t *testing.T) {
	url, loads := fakeOllama(t, `{"models":[{"name":"m:1b","context_length":32768}]}`)
	t.Setenv("OLLAMA_URL", url)
	t.Setenv("OLLAMA_MODEL", "m:1b")
	resetWarm()

	WarmAsync()
	waitIdle(t)

	if loads.Load() != 1 {
		t.Fatalf("a runner at the wrong num_ctx does not serve the agents; want 1 preload, got %d", loads.Load())
	}
}

func TestWarmAsyncIsDebouncedAndOffUntilEnabled(t *testing.T) {
	url, loads := fakeOllama(t, `{"models":[]}`)
	t.Setenv("OLLAMA_URL", url)
	t.Setenv("OLLAMA_MODEL", "m:1b")

	resetWarm()
	warmState.mu.Lock()
	warmState.enabled = false
	warmState.mu.Unlock()
	WarmAsync()
	time.Sleep(50 * time.Millisecond)
	if loads.Load() != 0 {
		t.Fatal("WarmAsync must do nothing until EnableWarm")
	}

	resetWarm()
	WarmAsync()
	waitIdle(t)
	WarmAsync() // inside warmMinGap: no second attempt
	time.Sleep(50 * time.Millisecond)
	if loads.Load() != 1 {
		t.Fatalf("second call inside the debounce window loaded again (%d)", loads.Load())
	}
}
