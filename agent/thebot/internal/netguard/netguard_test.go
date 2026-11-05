package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestPublicRefusesEveryNonPublicRange(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.8.9.10", "::1", // loopback (soul 4900, agent API, Chrome 9222)
		"10.1.2.3", "172.17.0.1", "192.168.65.254", // RFC 1918 (host.docker.internal, the decurix container)
		"169.254.169.254", "fe80::1", // link-local, cloud metadata
		"fd00::1", "fc12::5", // unique-local
		"0.0.0.0", "::", "0.1.2.3", // unspecified, "this network"
		"100.64.0.1", "100.127.255.254", // CGNAT
		"224.0.0.1", "ff02::1", "255.255.255.255",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", // IPv4-mapped
		"64:ff9b::a9fe:a9fe", // NAT64 of 169.254.169.254
	} {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("Public(%s) = true, want false", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "100.128.0.1", "2606:4700::1111"} {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("Public(%s) = false, want true", s)
		}
	}
}

func TestClientRefusesLoopbackAndMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("control plane")) }))
	defer srv.Close()
	c := Client(5 * time.Second)
	for _, u := range []string{srv.URL, "http://localhost:4900/api/chat", "http://169.254.169.254/latest/meta-data/", "http://[::1]:8080/", "file:///etc/passwd"} {
		resp, err := c.Get(u)
		if err == nil {
			resp.Body.Close()
			t.Errorf("Get(%s) succeeded, want refused", u)
		}
	}
}

// A name is checked at the dial against every address it resolves to - a public answer next to a private
// one (or a resolver that answers differently on the next lookup) never reaches the private address.
func TestDialChecksEveryResolvedAddress(t *testing.T) {
	g := &guard{
		allow: Public,
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
		},
		dialer: &net.Dialer{Timeout: time.Second},
	}
	if _, err := g.dial(context.Background(), "tcp", "rebind.example:80"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("dial = %v, want ErrBlocked", err)
	}
}

// A redirect from a page the client may fetch to one it may not is refused at the redirect.
func TestClientRechecksEveryRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		default:
			w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()

	srvAddr := netip.MustParseAddrPort(srv.Listener.Addr().String())
	// This test's stand-in for "the public internet" is the first server's port only.
	g := &guard{allow: func(a netip.Addr) bool { return a.Unmap() == srvAddr.Addr() }, resolve: defaultResolve, dialer: &net.Dialer{Timeout: time.Second}}
	c := g.client(5 * time.Second)
	// A redirect to an address literal that is not allowed is refused before it is dialled.
	if resp, err := c.Get(srv.URL + "/to-metadata"); err == nil {
		resp.Body.Close()
		t.Fatal("followed a redirect to the metadata address")
	} else if !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect error = %v, want ErrBlocked", err)
	}
	resp, err := c.Get(srv.URL + "/fine")
	if err != nil {
		t.Fatalf("Get allowed page: %v", err)
	}
	resp.Body.Close()
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8081/", "http://localhost/", "ftp://example.com/x", "http://10.0.0.5/", "http://[fd00::1]/"} {
		if CheckURL(u) == nil {
			t.Errorf("CheckURL(%s) = nil, want refused", u)
		}
	}
	if err := CheckURL("https://example.com/report.pdf"); err != nil {
		t.Errorf("CheckURL(public) = %v", err)
	}
}

// With a proxy configured, a request to a public target is routed through the proxy (a proxied network keeps its
// downloads and web search), but the proxy is never a way to reach a loopback, metadata or private target.
func TestProxyRoutesPublicTargetsButNotInternalOnes(t *testing.T) {
	var proxied int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied++; w.Write([]byte("via proxy")) }))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	proxyHost, proxyPort, _ := net.SplitHostPort(proxyURL.Host)

	g := &guard{
		allow: Public,
		resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "good.example":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			case "sneaky.internal":
				return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
			}
			return nil, errors.New("no such host")
		},
		dialer:     &net.Dialer{Timeout: 2 * time.Second},
		proxy:      func(*http.Request) (*url.URL, error) { return proxyURL, nil },
		proxyHosts: map[string]bool{proxyHost: true, net.JoinHostPort(proxyHost, proxyPort): true},
	}
	c := g.client(5 * time.Second)

	resp, err := c.Get("http://good.example/file.pdf")
	if err != nil {
		t.Fatalf("a public target was not fetched through the proxy: %v", err)
	}
	resp.Body.Close()
	if proxied != 1 {
		t.Fatalf("the request did not go through the proxy (proxied=%d)", proxied)
	}

	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/", // metadata literal
		"http://sneaky.internal/",                  // a name that resolves to metadata
		"http://localhost:4900/api/chat",           // localhost
		"http://127.0.0.1:8080/",                   // loopback literal
		"http://10.1.2.3/",                         // private literal
	} {
		if resp, err := c.Get(u); err == nil {
			resp.Body.Close()
			t.Errorf("Get(%s) via proxy succeeded, want refused", u)
		}
	}
	if proxied != 1 {
		t.Errorf("an internal target was sent to the proxy anyway (proxied=%d)", proxied)
	}
}

// The proxy endpoints are read from the proxy environment variables, with and without a port.
func TestProxyEndpointsFromEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.corp:3128")
	t.Setenv("HTTPS_PROXY", "squid.lan:8080")
	t.Setenv("NO_PROXY", "")
	if !proxyConfigured() {
		t.Fatal("proxyConfigured = false with a proxy set")
	}
	eps := proxyEndpoints()
	for _, want := range []string{"proxy.corp", "proxy.corp:3128", "squid.lan", "squid.lan:8080"} {
		if !eps[want] {
			t.Errorf("proxyEndpoints missing %q (got %v)", want, eps)
		}
	}
	g := &guard{proxyHosts: eps}
	if !g.isProxyEndpoint("proxy.corp", "3128") || !g.isProxyEndpoint("squid.lan", "8080") {
		t.Error("a configured proxy endpoint was not recognised")
	}
	if g.isProxyEndpoint("evil.example", "80") {
		t.Error("a non-proxy host was taken for the proxy")
	}
}

// Without a proxy configured, the client dials directly and keeps the public-only check (unchanged behaviour).
func TestNoProxyByDefault(t *testing.T) {
	for _, k := range proxyEnv {
		t.Setenv(k, "")
	}
	if proxyConfigured() {
		t.Fatal("proxyConfigured = true with no proxy set")
	}
}
