package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Engine  string `json:"engine"`
}

var (
	ddgRe   = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	ddgURL  = regexp.MustCompile(`uddg=([^&]+)`)
	bingRe  = regexp.MustCompile(`(?s)<li class="b_algo"[^>]*>.*?<h2[^>]*><a[^>]+href="([^"]+)"[^>]*>(.*?)</a></h2>(?:.{0,400}?<p[^>]*>(.*?)</p>)?`)
	bingURL = regexp.MustCompile(`[?&]u=a1([^&]+)`)
)

func (c *Client) endpoint(name, def string) string {
	if c.Engines != nil && c.Engines[name] != "" {
		return c.Engines[name]
	}
	return def
}

// Search asks the configured engines in turn (SearXNG, Brave, DuckDuckGo,
// Bing) and returns the first engine's results that has any, without ads
// and duplicates. Google is not used: it answers keyless clients with a
// CAPTCHA (seen live).
func (c *Client) Search(ctx context.Context, query string, n int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("web: empty search")
	}
	if n <= 0 {
		n = 8
	}
	type engine struct {
		name string
		run  func(context.Context, string) ([]Result, error)
	}
	var engines []engine
	if c.SearxURL != "" {
		engines = append(engines, engine{"searxng", c.searx})
	}
	if c.BraveKey != "" {
		engines = append(engines, engine{"brave", c.brave})
	}
	engines = append(engines, engine{"duckduckgo", c.duckduckgo}, engine{"bing", c.bing})
	var errs []string
	for _, e := range engines {
		res, err := e.run(ctx, query)
		if err != nil {
			errs = append(errs, e.name+": "+err.Error())
			continue
		}
		if out := tidy(res, n); len(out) > 0 {
			return out, nil
		}
		errs = append(errs, e.name+": no results")
	}
	return nil, fmt.Errorf("web: every search engine failed (%s)", strings.Join(errs, "; "))
}

// tidy drops ads, non-web links and repeats, and keeps n.
func tidy(in []Result, n int) []Result {
	seen := map[string]bool{}
	var out []Result
	for _, r := range in {
		if r.URL == "" || isAd(r.URL) || seen[r.URL] {
			continue
		}
		if u, err := url.Parse(r.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		seen[r.URL] = true
		out = append(out, r)
		if len(out) == n {
			break
		}
	}
	return out
}

func isAd(link string) bool {
	for _, m := range []string{"duckduckgo.com/y.js", "bing.com/aclick", "/aclk?", "googleadservices", "doubleclick.net"} {
		if strings.Contains(link, m) {
			return true
		}
	}
	return false
}

func (c *Client) page(ctx context.Context, target string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "en-IN,en;q=0.9")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, c.maxBytes()))
}

func (c *Client) duckduckgo(ctx context.Context, q string) ([]Result, error) {
	body, err := c.page(ctx, c.endpoint("duckduckgo", "https://html.duckduckgo.com/html/")+"?q="+url.QueryEscape(q), nil)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, m := range ddgRe.FindAllStringSubmatch(string(body), -1) {
		link := m[1]
		if u := ddgURL.FindStringSubmatch(link); u != nil {
			if d, err := url.QueryUnescape(u[1]); err == nil {
				link = d
			}
		}
		if strings.HasPrefix(link, "//") {
			link = "https:" + link
		}
		out = append(out, Result{Title: clean(m[2]), URL: link, Snippet: clean(m[3]), Engine: "duckduckgo"})
	}
	return out, nil
}

func (c *Client) bing(ctx context.Context, q string) ([]Result, error) {
	body, err := c.page(ctx, c.endpoint("bing", "https://www.bing.com/search")+"?q="+url.QueryEscape(q), nil)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, m := range bingRe.FindAllStringSubmatch(string(body), -1) {
		link := html2text(m[1])
		if u := bingURL.FindStringSubmatch(link); u != nil {
			if d, err := base64.RawURLEncoding.DecodeString(u[1]); err == nil {
				link = string(d)
			}
		}
		out = append(out, Result{Title: clean(m[2]), URL: link, Snippet: clean(m[3]), Engine: "bing"})
	}
	return out, nil
}

func html2text(s string) string { return strings.ReplaceAll(s, "&amp;", "&") }

func (c *Client) searx(ctx context.Context, q string) ([]Result, error) {
	body, err := c.page(ctx, strings.TrimRight(c.SearxURL, "/")+"/search?format=json&q="+url.QueryEscape(q), nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var out []Result
	for _, x := range r.Results {
		out = append(out, Result{Title: x.Title, URL: x.URL, Snippet: x.Content, Engine: "searxng"})
	}
	return out, nil
}

func (c *Client) brave(ctx context.Context, q string) ([]Result, error) {
	body, err := c.page(ctx, c.endpoint("brave", "https://api.search.brave.com/res/v1/web/search")+"?q="+url.QueryEscape(q),
		map[string]string{"X-Subscription-Token": c.BraveKey, "Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	var r struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var out []Result
	for _, x := range r.Web.Results {
		out = append(out, Result{Title: clean(x.Title), URL: x.URL, Snippet: clean(x.Description), Engine: "brave"})
	}
	return out, nil
}
