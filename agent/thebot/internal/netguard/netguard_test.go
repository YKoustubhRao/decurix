package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
