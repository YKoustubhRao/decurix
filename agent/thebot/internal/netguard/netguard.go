// Package netguard is the HTTP client every fetch of a model- or page-named
// URL goes through. A desktop shares a network with the control plane
// (host.docker.internal:8080, decurix:8080), with its own local services
// (soul on 4900, the agent API, Chrome's debugging port) and, on a cloud host,
// with the metadata service on 169.254.169.254. A URL a model or a web page
// chose must reach none of them: only public addresses are dialled.
//
// The check is made on the address actually dialled, after resolution, on
// every connection - so a redirect to a private address and a name that
// resolves to a public address for the check and a private one for the dial
// (DNS rebinding) are refused alike.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrBlocked is the error a refused address wraps.
var ErrBlocked = errors.New("refused: not a public internet address")

// blockedPrefixes are the ranges netip's own predicates do not cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT (shared address space)
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, and 255.255.255.255
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: may embed any IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4: embeds an IPv4 address
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
}

// Public reports whether addr is a public unicast address a fetch may dial:
// not loopback, private (RFC 1918, unique-local fc00::/7), link-local
// (169.254/16, fe80::/10 - the cloud metadata service), unspecified,
// multicast, carrier-grade NAT (100.64/10) or another reserved range.
func Public(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// guard is one client's policy: which addresses it may dial and how names resolve.
type guard struct {
	allow   func(netip.Addr) bool
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dialer  *net.Dialer
	// proxy, when set, is where a request is sent (honouring HTTP(S)_PROXY/NO_PROXY). The proxy then does the
	// dialing, so the public-address check can no longer be made on the target's own connection; instead the
	// target's name and IP are checked before the request is handed to the proxy (checkTarget), and the configured
	// proxy endpoints (proxyHosts) are the only non-public addresses dial lets through.
	proxy      func(*http.Request) (*url.URL, error)
	proxyHosts map[string]bool
}

func defaultResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dial resolves host, refuses it unless every address it resolves to is allowed, and dials one of
// those checked addresses itself - never the name again, so what was checked is what is reached.
func (g *guard) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	// A dial to the configured proxy itself is let through as given: the proxy is operator-configured infrastructure
	// (often on a private or loopback address), and it is the proxy that then reaches the - already checked - target.
	if g.isProxyEndpoint(host, port) {
		return g.dialer.DialContext(ctx, network, address)
	}
	var addrs []netip.Addr
	if ip, perr := netip.ParseAddr(strings.Trim(host, "[]")); perr == nil {
		addrs = []netip.Addr{ip}
	} else {
		addrs, err = g.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s: no address", host)
	}
	// Every address must be public: a name that resolves to one public and one
	// private address is refused, so no retry of the dial can land on the private one.
	for _, a := range addrs {
		if !g.allow(a) {
			return nil, fmt.Errorf("%s (%s): %w", host, a.Unmap(), ErrBlocked)
		}
	}
	var lastErr error
	for _, a := range addrs {
		conn, derr := g.dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

// checkURL refuses a URL that is not http(s), or whose host is an address literal that is not allowed
// (the dial check would refuse it too; this says why before any connection is tried).
func (g *guard) checkURL(u *url.URL) error {
	if u == nil {
		return errors.New("no URL")
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return fmt.Errorf("%s: only http and https links are fetched", u.Redacted())
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%s: no host", u.Redacted())
	}
	if ip, err := netip.ParseAddr(host); err == nil && !g.allow(ip) {
		return fmt.Errorf("%s: %w", host, ErrBlocked)
	}
	if h := strings.ToLower(strings.TrimSuffix(host, ".")); h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%s: %w", host, ErrBlocked)
	}
	return nil
}

// checkTarget checks a request's target before it is handed to a proxy. The proxy, not this client, dials the
// target, so the per-connection address check (dial) never sees it; the address-literal and localhost checks still
// hold, and a name is resolved here as well, best effort - a name this host can resolve to a loopback, private or
// metadata address is refused, so a proxy is never a way around the guard for a target this host can see. A name
// only the proxy can resolve is left to the proxy; that is the proxy's trust domain, not a bypass this host offers.
func (g *guard) checkTarget(ctx context.Context, u *url.URL) error {
	if err := g.checkURL(u); err != nil {
		return err
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return nil // a literal was already checked by checkURL
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return nil // only the proxy may be able to resolve it
	}
	for _, a := range addrs {
		if !g.allow(a) {
			return fmt.Errorf("%s (%s): %w", host, a.Unmap(), ErrBlocked)
		}
	}
	return nil
}

// maxRedirects is how many redirects one fetch follows.
const maxRedirects = 10

func (g *guard) client(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		// With no proxy configured, every connection is dialled directly and checked (Proxy nil). With one
		// configured (HTTP(S)_PROXY, honouring NO_PROXY), the proxy dials the target and the target is checked in
		// RoundTrip instead; a NO_PROXY match makes g.proxy return nil for that request, so it is dialled and
		// checked directly as before.
		Proxy:                 g.proxy,
		DialContext:           g.dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &checkedTransport{g: g, next: transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return g.checkURL(req.URL)
		},
	}
}

// checkedTransport checks each request's URL before the transport dials it.
type checkedTransport struct {
	g    *guard
	next http.RoundTripper
}

func (t *checkedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.g.proxy != nil {
		if err := t.g.checkTarget(req.Context(), req.URL); err != nil {
			return nil, err
		}
	} else if err := t.g.checkURL(req.URL); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

// Client is an HTTP client that dials only public internet addresses and
// checks every redirect. When HTTP_PROXY/HTTPS_PROXY (or ALL_PROXY) is set it
// routes through the proxy (honouring NO_PROXY) instead of dialling directly,
// so a proxied network keeps downloads and web search; the target is still
// checked first, and the proxy is never a way to reach a loopback, private or
// metadata target. timeout 0 means none beyond the request's context.
func Client(timeout time.Duration) *http.Client {
	g := &guard{allow: Public, resolve: defaultResolve, dialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}}
	if proxyConfigured() {
		g.proxy = http.ProxyFromEnvironment
		g.proxyHosts = proxyEndpoints()
	}
	return g.client(timeout)
}

// proxyEnv are the environment variables that configure an outbound proxy.
var proxyEnv = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"}

// proxyConfigured reports whether any proxy environment variable names a proxy.
func proxyConfigured() bool {
	for _, k := range proxyEnv {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// proxyEndpoints is the set of host and host:port strings the configured proxies listen on, so a dial to the proxy
// is recognised and let through while every other direct dial stays public-only.
func proxyEndpoints() map[string]bool {
	out := map[string]bool{}
	for _, k := range proxyEnv {
		raw := strings.TrimSpace(os.Getenv(k))
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if h := strings.ToLower(u.Hostname()); h != "" {
			out[h] = true
			if p := u.Port(); p != "" {
				out[net.JoinHostPort(h, p)] = true
			}
		}
	}
	return out
}

// isProxyEndpoint reports whether host:port is one of the configured proxies.
func (g *guard) isProxyEndpoint(host, port string) bool {
	if len(g.proxyHosts) == 0 {
		return false
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	return g.proxyHosts[h] || g.proxyHosts[net.JoinHostPort(h, port)]
}

// CheckURL refuses a URL a guarded client would refuse before resolving it
// (not http(s), localhost, a non-public address literal). A name is checked
// only when it is dialled.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	return (&guard{allow: Public}).checkURL(u)
}
