// Package websearch is real HTML-scraping search against engines that
// actually return result markup to a plain HTTP GET - no API key, no
// headless browser. Ported from the Node prototype's lib/search-engines.js,
// including the lessons its comments recorded from real live failures.
//
// Google is deliberately absent: a live test with a realistic UA got an
// immediate CAPTCHA wall on the first request. That's active bot
// detection, not a parsing gap, and isn't something a keyless client gets
// past reliably.
package websearch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// declaredUserAgent identifies this agent honestly rather than impersonating
// a browser - some sites require exactly that. Found live: sec.gov hard
// 403s EVERY request carrying a browser-style UA with
// "Your Request Originates from an Undeclared Automated Tool", and its own
// published access policy (sec.gov/developer) asks for precisely this
// format - "Company/App Name contact-email". Declaring identity here is
// compliance with the site's stated policy, not evasion of it - evasion
// would mean impersonating a browser, which is what the browser UA above
// already does for search engines that require it for unrelated reasons
// (they serve stripped-down HTML to non-browser UAs).
const declaredUserAgent = "TheBot-Research/1.0 (automated research agent; contact: research@thebot.local)"

type Result struct {
	Title   string
	URL     string
	Snippet string
	Engine  string
}

var (
	tagRe    = regexp.MustCompile(`(?s)<[^>]+>`)
	scriptRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	wsRe     = regexp.MustCompile(`\s+`)

	ddgRe  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	ddgURL = regexp.MustCompile(`uddg=([^&]+)`)

	bingRe  = regexp.MustCompile(`(?s)<li class="b_algo"[^>]*>.*?<h2[^>]*><a[^>]+href="([^"]+)"[^>]*>(.*?)</a></h2>(?:.{0,300}?<p[^>]*>(.*?)</p>)?`)
	bingURL = regexp.MustCompile(`[?&]u=a1([^&]+)`)
)

// isAd filters out the engines' own sponsored-link redirects, which
// otherwise show up as the top "result" and are never what was asked for.
func isAd(link string) bool {
	for _, marker := range []string{"duckduckgo.com/y.js", "bing.com/aclick", "/aclk?", "googleadservices"} {
		if strings.Contains(link, marker) {
			return true
		}
	}
	return false
}

func stripHTML(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	r := strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#x27;", "'", "&#39;", "'",
	)
	s = r.Replace(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// fetchHTML requires an exact HTTP 200. Checking "any 2xx" isn't enough -
// caught live in the Node prototype: DuckDuckGo rate-limiting back-to-back
// requests returned a real HTTP 202 (which IS "ok") carrying its plain
// homepage instead of results.
func fetchHTML(ctx context.Context, target, label, ua string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ua)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", &httpStatusError{label: label, status: resp.StatusCode}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// httpStatusError carries the real status code forward so a caller can
// decide to retry with a different identity (403 specifically) rather
// than just failing - a plain fmt.Errorf loses that.
type httpStatusError struct {
	label  string
	status int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d (may be rate-limited or blocked)", e.label, e.status)
}

// DuckDuckGo searches DDG's HTML endpoint. Page 1 only - DDG's paging
// needs a stateful POST chain carrying its own per-query vqd token, which
// isn't worth the complexity here.
func DuckDuckGo(ctx context.Context, query string) ([]Result, error) {
	html, err := fetchHTML(ctx, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(query), "duckduckgo", userAgent)
	if err != nil {
		return nil, err
	}

	var out []Result
	for _, m := range ddgRe.FindAllStringSubmatch(html, -1) {
		link := m[1]
		if u := ddgURL.FindStringSubmatch(link); u != nil {
			if decoded, err := url.QueryUnescape(u[1]); err == nil {
				link = decoded
			}
		}
		link = strings.TrimPrefix(link, "//")
		if !strings.HasPrefix(link, "http") {
			link = "https://" + link
		}
		if isAd(link) {
			continue // DDG's own y.js ad redirects - real results only
		}
		out = append(out, Result{Title: stripHTML(m[2]), URL: link, Snippet: stripHTML(m[3]), Engine: "duckduckgo"})
	}

	// Zero results for an ordinary query almost always means a soft
	// block that still returned 200, not a genuine "nothing found" - real
	// engines essentially never return truly zero for a normal query.
	// Surfacing that distinction beats silently reporting "no results".
	if len(out) == 0 {
		return nil, fmt.Errorf("duckduckgo returned 200 with no parseable results - likely blocked or its HTML changed")
	}
	return out, nil
}

// Bing is the fallback when DuckDuckGo refuses.
func Bing(ctx context.Context, query string) ([]Result, error) {
	html, err := fetchHTML(ctx, "https://www.bing.com/search?q="+url.QueryEscape(query), "bing", userAgent)
	if err != nil {
		return nil, err
	}

	var out []Result
	for _, m := range bingRe.FindAllStringSubmatch(html, -1) {
		// The href in raw HTML has its query-string "&" entity-encoded as
		// "&amp;" - matching [?&]u=a1 against the raw string never finds
		// it, which silently fell through to the tracking URL instead of
		// the real one (a real bug found in the Node prototype).
		href := strings.ReplaceAll(m[1], "&amp;", "&")
		if u := bingURL.FindStringSubmatch(href); u != nil {
			if decoded, err := decodeBingURL(u[1]); err == nil && strings.HasPrefix(decoded, "http") {
				href = decoded
			}
		}
		out = append(out, Result{Title: stripHTML(m[2]), URL: href, Snippet: stripHTML(m[3]), Engine: "bing"})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bing returned 200 with no parseable results - likely blocked or its HTML changed")
	}
	return out, nil
}

// decodeBingURL undoes Bing's "u=a1<base64url>" redirect wrapper. Bing
// omits padding, so RawURLEncoding is the right decoder here.
func decodeBingURL(s string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// Search tries each engine in turn and returns the first real set of
// results, along with which engine actually produced them. Every engine
// failing is a real error, not an empty list.
func Search(ctx context.Context, query string, onLog func(string)) ([]Result, error) {
	type engine struct {
		name string
		fn   func(context.Context, string) ([]Result, error)
	}
	var problems []string
	for _, e := range []engine{{"duckduckgo", DuckDuckGo}, {"bing", Bing}} {
		if onLog != nil {
			onLog("websearch: querying " + e.name + " for: " + query)
		}
		results, err := e.fn(ctx, query)
		if err != nil {
			problems = append(problems, err.Error())
			if onLog != nil {
				onLog("websearch: " + e.name + " failed: " + err.Error())
			}
			continue
		}
		if onLog != nil {
			onLog(fmt.Sprintf("websearch: %s returned %d real result(s)", e.name, len(results)))
		}
		return results, nil
	}
	return nil, fmt.Errorf("every search engine failed: %s", strings.Join(problems, " | "))
}

// FetchPageText fetches one real page and strips it to readable text -
// the "deep read" step that turns a search hit into actual content the
// model can reason over, rather than just a snippet.
//
// Tries as a browser first (what most sites expect), and on an HTTP 403
// retries once declaring this agent's real identity instead - some sites
// (sec.gov confirmed live) block browser-style UAs specifically and only
// serve non-browser clients that declare themselves, per their own
// published access policy. A 403 from a plain "curl-like" GET, as opposed
// to active bot-detection (a CAPTCHA page, a 200 with a JS challenge),
// specifically means "identify yourself" here, not "go away".
func FetchPageText(ctx context.Context, target string, maxChars int) (string, error) {
	html, err := fetchHTML(ctx, target, "page", userAgent)
	if err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) && statusErr.status == http.StatusForbidden {
			html, err = fetchHTML(ctx, target, "page", declaredUserAgent)
		}
		if err != nil {
			return "", err
		}
	}
	text := stripHTML(html)
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	return text, nil
}
