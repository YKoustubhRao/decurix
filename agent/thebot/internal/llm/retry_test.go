package llm

import (
	"context"
	"errors"
	"io"
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
	now := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		key, value string
		want       time.Duration
	}{
		{"Retry-After-Ms", "250", 250 * time.Millisecond},
		{"Retry-After", "3", 3 * time.Second},
		{"Retry-After", now.Add(7 * time.Second).Format(http.TimeFormat), 7 * time.Second},
		{"Retry-After", "invalid", 2 * time.Second},
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
	for attempt, want := range map[int]time.Duration{0: 2 * time.Second, 2: 8 * time.Second, 4: 30 * time.Second, 9: 30 * time.Second} {
		if got := retryDelay(http.Header{}, time.Now(), attempt); got != want {
			t.Errorf("attempt %d: %v, want %v", attempt, got, want)
		}
	}
}
