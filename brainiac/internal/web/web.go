// Package web is Brainiac's browser: search engines, page fetching, and a real
// headless browser for pages that need JavaScript. Two things use it: answers
// that need facts newer than a model's training (web evidence), and the scout
// that keeps the model catalog current.
//
// It is built to be pointed at the open internet by a model:
//   - it never dials a private, loopback, link-local or multicast address (the
//     check runs on the address actually dialed, so a DNS answer cannot
//     redirect it inside the network), and follows at most 5 redirects;
//   - it reads at most MaxBytes of a page and gives up after Timeout;
//   - what it returns is page text for the brain to read as evidence, never
//     instructions;
//   - search queries are scrubbed of IDs and secrets before they leave (Scrub).
//
// Depends on nothing internal.
package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// UserAgent identifies Brainiac honestly; search engines get a browser one
// because they serve stripped pages to anything else.
const UserAgent = "Brainiac/1.0 (local model router; automated reader)"

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// ErrBlockedAddress is a URL that resolves to an address Brainiac never dials.
var ErrBlockedAddress = errors.New("web: that address is inside this machine or its network, which the browser never reads")

// Client is the browser.
type Client struct {
	HTTP     *http.Client
	MaxBytes int64
	Timeout  time.Duration
	// SearxURL is a SearXNG instance to search with first (its JSON API).
	SearxURL string
	// BraveKey is a Brave Search API key, tried before the scraped engines.
	BraveKey string
	// Browser is a Chrome/Chromium/Edge executable for Render; "" finds one.
	Browser string
	// AllowPrivate lifts the address guard (tests only).
	AllowPrivate bool
	// Engines overrides search engine endpoints by name (duckduckgo, bing,
	// brave): a self-hosted mirror, or a stand-in in tests.
	Engines map[string]string
}

// New returns a browser with safe defaults.
func New() *Client {
	c := &Client{MaxBytes: 3 << 20, Timeout: 25 * time.Second}
	c.HTTP = c.client()
	return c
}

// dialer dials only addresses the browser may read, checked on the address
// actually dialed.
func (c *Client) dialer() *net.Dialer {
	return &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		if c.AllowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || blocked(ip) {
			return ErrBlockedAddress
		}
		return nil
	}}
}

func (c *Client) client() *http.Client {
	tr := &http.Transport{DialContext: c.dialer().DialContext, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second}
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("web: too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("web: redirect to a non-web address")
		}
		return nil
	}}
}

// blocked reports whether an IP is one the browser must never reach.
func blocked(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() ||
		// carrier-grade NAT and the cloud metadata range
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xC0 == 64) || ip.Equal(net.IPv4(169, 254, 169, 254))
}

// checkURL refuses anything but a plain http(s) URL without credentials.
func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("web: %q is not an http(s) address", raw)
	}
	if u.User != nil {
		return nil, errors.New("web: addresses with credentials in them are refused")
	}
	return u, nil
}

// Page is what was read from one address.
type Page struct {
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Text      string    `json:"text"`
	Published string    `json:"published,omitempty"`
	Retrieved time.Time `json:"retrieved"`
	// Rendered says a headless browser ran the page's JavaScript.
	Rendered bool   `json:"rendered,omitempty"`
	Type     string `json:"type"`
}

// Fetch reads one page as text. A 403 is retried once with Brainiac's
// declared identity, which some sites require (sec.gov refuses browser-like
// agents from automated tools, and says so in its access policy).
func (c *Client) Fetch(ctx context.Context, raw string) (Page, error) {
	u, err := checkURL(raw)
	if err != nil {
		return Page{}, err
	}
	body, final, ctype, err := c.get(ctx, u.String(), browserUA)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusForbidden {
		body, final, ctype, err = c.get(ctx, u.String(), UserAgent)
	}
	if err != nil {
		return Page{}, err
	}
	return pageFrom(final, ctype, body), nil
}

// FetchRaw reads an address's bytes as they are (CSV, JSON, a zip of data),
// with the same address guard and up to 16 MB.
func (c *Client) FetchRaw(ctx context.Context, raw string) ([]byte, error) {
	u, err := checkURL(raw)
	if err != nil {
		return nil, err
	}
	saved := c.MaxBytes
	c.MaxBytes = 16 << 20
	defer func() { c.MaxBytes = saved }()
	body, _, _, err := c.get(ctx, u.String(), UserAgent)
	return body, err
}

type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("web: HTTP %d", e.code) }

func (c *Client) get(ctx context.Context, target, ua string) ([]byte, string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-IN,en;q=0.9")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return nil, "", "", ErrBlockedAddress
		}
		return nil, "", "", fmt.Errorf("web: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", &statusError{resp.StatusCode}
	}
	ctype := resp.Header.Get("Content-Type")
	if strings.Contains(ctype, "pdf") || strings.HasPrefix(ctype, "image/") || strings.HasPrefix(ctype, "audio/") || strings.HasPrefix(ctype, "video/") {
		return nil, "", "", fmt.Errorf("web: %s pages are not read as text", ctype)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes()))
	if err != nil {
		return nil, "", "", fmt.Errorf("web: reading the page: %w", err)
	}
	return body, resp.Request.URL.String(), ctype, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = c.client()
	}
	return c.HTTP
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 25 * time.Second
}

func (c *Client) maxBytes() int64 {
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return 3 << 20
}

var (
	dropRe      = regexp.MustCompile(`(?is)<(script|style|noscript|svg|template|iframe|head|nav|footer|form|button|select)\b[^>]*>.*?</(script|style|noscript|svg|template|iframe|head|nav|footer|form|button|select)>|<!--.*?-->`)
	titleRe     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	h1Re        = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	pubRe       = regexp.MustCompile(`(?i)<meta[^>]+(?:property|name|itemprop)=["'](?:article:published_time|datePublished|date|pubdate|og:updated_time|article:modified_time)["'][^>]+content=["']([^"']+)["']`)
	blockRe     = regexp.MustCompile(`(?i)</?(p|div|section|article|main|br|li|ul|ol|tr|table|h[1-6]|pre|blockquote|dd|dt|header|aside)\b[^>]*>`)
	cellRe      = regexp.MustCompile(`(?i)</?(td|th)\b[^>]*>`)
	tagRe       = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRe     = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankRe     = regexp.MustCompile(`\n\s*\n+`)
	shortLineRe = regexp.MustCompile(`(?m)^.{0,2}$\n?`)
)

// pageFrom turns a fetched body into readable text.
func pageFrom(final, ctype string, body []byte) Page {
	p := Page{URL: final, Retrieved: time.Now().UTC(), Type: ctype}
	if !strings.Contains(ctype, "html") && !bytes.Contains(bytes.ToLower(body[:min(len(body), 512)]), []byte("<html")) {
		p.Text = strings.TrimSpace(string(body))
		p.Type = "text"
		return p
	}
	s := string(body)
	if m := titleRe.FindStringSubmatch(s); m != nil {
		p.Title = clean(m[1])
	}
	if p.Title == "" {
		if m := h1Re.FindStringSubmatch(s); m != nil {
			p.Title = clean(m[1])
		}
	}
	if m := pubRe.FindStringSubmatch(s); m != nil {
		p.Published = m[1]
	}
	p.Text = HTMLText(s)
	p.Type = "html"
	return p
}

// HTMLText is the readable text of an HTML document: scripts, styles,
// navigation and forms dropped, blocks on their own lines, table cells
// separated, entities decoded.
func HTMLText(s string) string {
	s = dropRe.ReplaceAllString(s, " ")
	s = cellRe.ReplaceAllString(s, " | ")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = spaceRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = shortLineRe.ReplaceAllString(s, "")
	s = blankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func clean(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(html.UnescapeString(tagRe.ReplaceAllString(s, " ")), " "))
}
