package llm

import (
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRateLimitRetries bounds how often one call is sent again after a 429. The call's own deadline is the real limit:
// a hosted model's per-minute token quota is a one-minute window, and four tries (about 30 s of waiting with no hint)
// gave up on a busy minute while two thirds of the operator's 180 s deadline was left - found live, an application bill on
// Azure ended "exceeded token rate limit" twice.
// maxRateLimitRetries is high because a 429 says nothing about the work: a worker that shares a token quota waits
// its turn (about 30 seconds each, within the job's deadline) and never fails a task over it. Found 2026-10-30: at 10,
// about five minutes, one busy deployment could end a good run.
const maxRateLimitRetries = 60

// maxFallbackDelay caps the wait when the provider gives no hint.
const maxFallbackDelay = 30 * time.Second

// maxRetryAfter caps the wait a provider's own hint (Retry-After and its millisecond forms) may ask for: a hint of
// minutes is waited out a minute at a time, each try counted, rather than as one long wait (Hermes
// agent/retry_utils.py caps it the same way). A hint used to be ignored above 240 s, and the fallback used instead.
const maxRetryAfter = 60 * time.Second

// jitter spreads a fallback wait d over [d/2, d], so desktops refused by one provider at the same moment do not all
// ask again at the same moment (a variable so tests can pin it).
var jitter = func(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	return d/2 + rand.N(d/2+1)
}

// maxServerErrorRetries bounds how often one call is sent again after the provider itself failed (500, 502, 503,
// 504, 529). Found by the GUI benchmark: a single "HTTP 500: Internal server error" from a hosted model, and a gateway's
// "upstream connect error" a few minutes later, each ended a task half done - a find-and-replace with its dialog
// open - as "a step failed in a way retrying cannot fix". A chat call changes nothing anywhere, so asking again is
// safe; a provider that is really down still fails, three tries and about fifteen seconds later.
const maxServerErrorRetries = 3

// maxDialRetries bounds how often a call whose connection could not be made is tried again (2, 4, 8, 16, 32 s:
// about a minute, what an endpoint restarting takes).
const maxDialRetries = 5

// neverConnected reports whether err is a failure to make the connection at all - refused, or a name that does not
// resolve - so the request reached nobody and sending it again cannot do anything twice.
func neverConnected(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return false
}

// transientServerError reports whether status is the provider's own passing failure: 500, 502, 503, 504, or 529 (too
// busy to take the call). Any other 4xx but 429 refuses the request itself and is never asked again.
func transientServerError(status int) bool {
	return status == http.StatusInternalServerError || status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout || status == statusOverloaded
}

// statusOverloaded is the status a provider answers when it is too busy to take the call (not an IANA code).
const statusOverloaded = 529

// rateLimitedRequest retries a rejected chat request within its existing deadline: a 429 as long as the deadline
// allows, the provider's own failure a few times. No successful call or uncertain network failure is repeated.
func rateLimitedRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	serverErrors, dialErrors := 0, 0
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
		if err != nil {
			// A connection that was never made (refused, or a name that does not resolve) sent nothing: the endpoint
			// is restarting. Found live (2026-10-15): a worker's model call met the control plane's restart ("lookup
			// decurix: no such host") and the task ended as "a step failed in a way retrying cannot fix".
			if neverConnected(err) && dialErrors < maxDialRetries {
				dialErrors++
				wait := time.Duration(1<<dialErrors) * time.Second
				if deadline, ok := req.Context().Deadline(); ok && time.Until(deadline) <= wait {
					return resp, err
				}
				timer := time.NewTimer(wait)
				select {
				case <-req.Context().Done():
					timer.Stop()
					return nil, req.Context().Err()
				case <-timer.C:
				}
				continue
			}
			return resp, err
		}
		if transientServerError(resp.StatusCode) {
			if serverErrors >= maxServerErrorRetries {
				return resp, nil
			}
			serverErrors++
		} else if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRateLimitRetries {
			return resp, nil
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

// retryDelay honors the provider's millisecond, second or HTTP-date retry hint, capped at maxRetryAfter; with no
// usable hint it backs off from 2 s, doubling, capped at maxFallbackDelay and jittered.
func retryDelay(header http.Header, now time.Time, attempt int) time.Duration {
	for _, key := range []string{"Retry-After-Ms", "X-Ms-Retry-After-Ms"} {
		if ms, err := strconv.ParseFloat(header.Get(key), 64); err == nil && ms >= 0 {
			return hinted(ms * float64(time.Millisecond))
		}
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return hinted(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := when.Sub(now); delay >= 0 {
			return min(delay, maxRetryAfter)
		}
	}
	return jitter(min(time.Second*time.Duration(2<<min(attempt, 5)), maxFallbackDelay))
}

// hinted is a hint of ns nanoseconds as a wait, capped at maxRetryAfter (an absurd or infinite hint included).
func hinted(ns float64) time.Duration {
	if ns >= float64(maxRetryAfter) {
		return maxRetryAfter
	}
	return time.Duration(ns)
}
