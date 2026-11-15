package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatWaitsForRateLimitAndResendsTheSameRequest(t *testing.T) {
	calls := 0
	var first string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if calls == 1 {
			first = string(data)
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(429)
			io.WriteString(w, "{\"error\":{\"message\":\"rate limit\"}}")
			return
		}
		if string(data) != first || r.Header.Get("Authorization") != "Bearer test" {
			t.Error("retry changed request")
		}
		io.WriteString(w, "{\"choices\":[{\"message\":{\"content\":\"done\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}")
	}))
	defer server.Close()
	p := NewOpenAI("test", "")
	p.BaseURL = server.URL
	result, err := p.Chat(context.Background(), "demo", []Message{{Role: "user", Content: "hello"}}, Options{})
	if err != nil || result.Text != "done" || calls != 2 || result.PromptTokens != 5 {
		t.Fatalf("result %+v, err %v, calls %d", result, err, calls)
	}
}
func TestRateLimitRetriesStopAndLeaveOtherErrorsAlone(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After-Ms", "0")
				w.WriteHeader(status)
				io.WriteString(w, "{\"error\":{\"message\":\"rejected\"}}")
			}))
			defer server.Close()
			p := NewOpenAI("test", "")
			p.BaseURL = server.URL
			_, err := p.Chat(context.Background(), "demo", nil, Options{})
			want := 1
			if status == 429 {
				want = maxRateLimitRetries + 1
			}
			if status == 500 {
				want = maxServerErrorRetries + 1 // the provider's own failure is asked again a few times
			}
			if err == nil || calls != want || !strings.Contains(err.Error(), "rejected") {
				t.Fatalf("calls %d, err %v", calls, err)
			}
		})
	}
}
func TestRateLimitWaitCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		cancel()
	}))
	defer server.Close()
	p := NewOpenAI("test", "")
	p.BaseURL = server.URL
	_, err := p.Chat(ctx, "demo", nil, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
func TestRetryHintsAndDeadline(t *testing.T) {
	pinJitter(t)
	now := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		key, value string
		want       time.Duration
	}{
		{"Retry-After-Ms", "250", 250 * time.Millisecond},
		{"Retry-After", "3", 3 * time.Second},
		{"Retry-After", now.Add(7 * time.Second).Format(http.TimeFormat), 7 * time.Second},
		{"Retry-After", "invalid", 2 * time.Second},
		// A hint of minutes is waited out a minute at a time (maxRetryAfter), never ignored.
		{"Retry-After", "600", maxRetryAfter},
		{"Retry-After-Ms", "900000", maxRetryAfter},
		{"Retry-After", "+Inf", maxRetryAfter},
		{"Retry-After", now.Add(10 * time.Minute).Format(http.TimeFormat), maxRetryAfter},
	} {
		header := http.Header{}
		header.Set(sample.key, sample.value)
		if got := retryDelay(header, now, 0); got != sample.want {
			t.Errorf("%s: %v", sample.value, got)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		io.WriteString(w, "{\"error\":{\"message\":\"quota\"}}")
	}))
	defer server.Close()
	p := NewOpenAI("test", "")
	p.BaseURL = server.URL
	_, err := p.Chat(context.Background(), "demo", nil, Options{TimeoutMs: 1000})
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err %v", err)
	}
}

func TestTheFallbackWaitIsCapped(t *testing.T) {
	pinJitter(t)
	for attempt, want := range map[int]time.Duration{0: 2 * time.Second, 2: 8 * time.Second, 4: 30 * time.Second, 9: 30 * time.Second} {
		if got := retryDelay(http.Header{}, time.Now(), attempt); got != want {
			t.Errorf("attempt %d: %v, want %v", attempt, got, want)
		}
	}
}

// One failure of the provider's own does not end a task: the call is sent again and its answer used.
func TestAProvidersOwnFailureIsAskedAgain(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After-Ms", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "upstream connect error or disconnect/reset before headers")
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()
	p := NewOpenAI("test", "")
	p.BaseURL = server.URL
	out, err := p.Chat(context.Background(), "demo", nil, Options{})
	if err != nil || calls != 2 || out.Text != "ok" {
		t.Fatalf("calls %d, out %+v, err %v", calls, out, err)
	}
}

// The control plane answers a desktop it could not place (Docker did not answer) 503 "try again", and a caller it
// knows is no desktop 403 (found live 2026-11-11). The first is asked again and its answer used; the second is final.
func TestTheControlPlanesTryAgainIsAskedAgainItsRefusalIsNot(t *testing.T) {
	for _, c := range []struct {
		status    int
		failFirst int
		wantCalls int
		wantOK    bool
	}{
		{http.StatusServiceUnavailable, 2, 3, true},
		{http.StatusForbidden, 2, 1, false},
	} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls <= c.failFirst {
				w.Header().Set("Retry-After-Ms", "0")
				w.WriteHeader(c.status)
				io.WriteString(w, `{"error":"the desktop list could not be read; try again"}`)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}))
		p := NewOpenAI("test", "")
		p.BaseURL = server.URL
		out, err := p.Chat(context.Background(), "demo", nil, Options{})
		server.Close()
		if calls != c.wantCalls || (err == nil) != c.wantOK || c.wantOK && out.Text != "ok" {
			t.Errorf("HTTP %d: calls %d (want %d), out %+v, err %v", c.status, calls, c.wantCalls, out, err)
		}
		if !c.wantOK && Transient(err) {
			t.Errorf("HTTP %d was taken as transient: %v", c.status, err)
		}
	}
}

func TestACallThatCouldNotConnectIsTriedAgain(t *testing.T) {
	// A listener that is not there yet: the first two dials are refused, then it answers.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	served := make(chan struct{})
	go func() {
		time.Sleep(2500 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			close(served)
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })}
		go srv.Serve(l)
		<-served
		srv.Close()
	}()
	defer close(served)
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader("{}"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("{}")), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := rateLimitedRequest(http.DefaultClient, req.WithContext(ctx))
	if err != nil {
		t.Fatalf("not retried: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// pinJitter makes the fallback wait exact for the rest of the test.
func pinJitter(t *testing.T) {
	t.Helper()
	saved := jitter
	jitter = func(d time.Duration) time.Duration { return d }
	t.Cleanup(func() { jitter = saved })
}

// With no hint the wait is spread over [d/2, d]: desktops refused at one moment do not all ask again at one moment.
func TestTheFallbackWaitIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 200 {
		got := retryDelay(http.Header{}, time.Now(), 2) // 8 s before jitter
		if got < 4*time.Second || got > 8*time.Second {
			t.Fatalf("wait %v outside [4s, 8s]", got)
		}
		seen[got] = true
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct waits in 200: not jittered", len(seen))
	}
}

// Every provider waits out a 429 and the provider's own failure (5xx, 529 overloaded) as its Retry-After says, and
// never asks again after any other 4xx.
func TestEveryProviderHonoursRetryAfterAndLeavesOtherRefusalsAlone(t *testing.T) {
	answers := map[string]string{
		"openai":    `{"choices":[{"message":{"content":"ok"}}]}`,
		"anthropic": `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		"ollama":    `{"message":{"content":"ok"},"done":true}`,
	}
	for _, name := range []string{"openai", "anthropic", "ollama"} {
		for _, c := range []struct {
			status    int
			wantCalls int
		}{{429, 2}, {503, 2}, {529, 2}, {400, 1}, {404, 1}, {422, 1}} {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(c.status)
					io.WriteString(w, `{"error":{"message":"no"}}`)
					return
				}
				io.WriteString(w, answers[name])
			}))
			var p Provider
			switch name {
			case "openai":
				o := NewOpenAI("k", "")
				o.BaseURL = srv.URL
				p = o
			case "anthropic":
				a := NewAnthropic("k", "m")
				a.BaseURL = srv.URL
				p = a
			default:
				o := NewOllama(srv.URL)
				o.TimeoutFloor = 0
				p = o
			}
			out, err := p.Chat(context.Background(), "m", []Message{{Role: "user", Content: "hi"}}, Options{TimeoutMs: 20000})
			srv.Close()
			if calls != c.wantCalls {
				t.Errorf("%s, HTTP %d: %d calls, want %d (err %v)", name, c.status, calls, c.wantCalls, err)
			}
			if c.wantCalls == 2 && (err != nil || out.Text != "ok") {
				t.Errorf("%s, HTTP %d: the second ask's answer was not used: %+v %v", name, c.status, out, err)
			}
			if c.wantCalls == 1 && err == nil {
				t.Errorf("%s, HTTP %d: a refusal was not reported", name, c.status)
			}
		}
	}
}
