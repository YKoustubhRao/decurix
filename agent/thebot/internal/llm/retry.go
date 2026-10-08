package llm

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRateLimitRetries bounds how often one call is sent again after a 429. The call's own deadline is the real limit:
// a hosted model's per-minute token quota is a one-minute window, and four tries (about 30 s of waiting with no hint)
// gave up on a busy minute while two thirds of the operator's 180 s deadline was left - found live, an application bill on
// Azure ended "exceeded token rate limit" twice.
const maxRateLimitRetries = 10

// maxFallbackDelay caps the wait when the provider gives no hint.
const maxFallbackDelay = 30 * time.Second

// rateLimitedRequest retries a rejected chat request within its existing deadline.
// Only 429 is retried: no successful call or uncertain network failure is repeated.
func rateLimitedRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		current := req.Clone(req.Context())
		if attempt > 0 {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			current.Body = body
		}
		resp, err := client.Do(current)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRateLimitRetries {
			return resp, err
		}
		delay := retryDelay(resp.Header, time.Now(), attempt)
		// If the provider asks for longer than our deadline, return its useful 429.
		if deadline, ok := req.Context().Deadline(); ok && time.Until(deadline) <= delay {
			return resp, nil
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
}

// retryDelay honors the provider's millisecond, second or HTTP-date retry hint.
func retryDelay(header http.Header, now time.Time, attempt int) time.Duration {
	for _, key := range []string{"Retry-After-Ms", "X-Ms-Retry-After-Ms"} {
		if ms, err := strconv.ParseFloat(header.Get(key), 64); err == nil && ms >= 0 && ms <= 240000 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 && seconds <= 240 {
		return time.Duration(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := when.Sub(now); delay >= 0 {
			return delay
		}
	}
	return min(time.Second*time.Duration(2<<min(attempt, 5)), maxFallbackDelay)
}
