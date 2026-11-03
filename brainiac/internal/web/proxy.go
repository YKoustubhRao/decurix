package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// guardProxy is a forward proxy on loopback through which the headless
// browser reaches the web. It dials with the browser client's own guard, on
// the address actually dialed, so the page, its redirects and everything it
// loads are held to what Fetch is held to: no loopback, private, link-local
// or metadata address, whatever a DNS answer or a redirect says.
type guardProxy struct {
	c     *Client
	mu    sync.Mutex
	conns map[net.Conn]bool // tunnels, closed by stop
}

func (g *guardProxy) track(cs ...net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range cs {
		g.conns[c] = true
	}
}

// startProxy runs a guard proxy until stop is called.
func (c *Client) startProxy() (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	g := &guardProxy{c: c, conns: map[net.Conn]bool{}}
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return ln.Addr().String(), func() {
		_ = srv.Close()
		g.mu.Lock()
		for c := range g.conns {
			c.Close()
		}
		g.mu.Unlock()
	}, nil
}

func (g *guardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		g.tunnel(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "only http and https", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Proxy-Authorization")
	tr := &http.Transport{DialContext: g.c.dialer().DialContext, ResponseHeaderTimeout: 20 * time.Second}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "refused", http.StatusForbidden)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, g.c.maxBytes()))
}

// tunnel serves CONNECT (https): it dials the target through the guard, then
// copies bytes both ways.
func (g *guardProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	up, err := g.c.dialer().DialContext(ctx, "tcp", r.Host)
	if err != nil {
		http.Error(w, "refused", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "no tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	g.track(up, down)
	_, _ = down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		_, _ = io.Copy(up, buf)
		up.Close()
	}()
	go func() {
		_, _ = io.Copy(down, up)
		down.Close()
	}()
}
