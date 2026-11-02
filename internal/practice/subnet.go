// Package practice runs Simulate (docs/simulation.md): it drafts a practice
// suite for a worker's taught skill or recording with the hosted heavy model,
// runs the worker itself in a disposable practice desktop against stand-ins,
// and decides every case in code (qualification.VerifyPractice). A pass is
// evidence for a person to review; nothing here enables anything.
package practice

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// InSubnets reports whether remoteAddr ("ip:port" or a bare IP) falls in any prefix.
func InSubnets(remoteAddr string, prefixes []netip.Prefix) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// RefuseSubnets answers 403 to any request from the practice network, so a
// practice desktop reaches only its broker, never the primary or gated listener.
// It reads the connection's address, never a forwarding header.
func RefuseSubnets(subnets func() []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if InSubnets(r.RemoteAddr, subnets()) {
			http.Error(w, "Practice desktops reach only their broker.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SubnetCache remembers the practice network's ranges for a short while, so the
// listener guard does not ask Docker on every request.
type SubnetCache struct {
	Lookup func(ctx context.Context) []netip.Prefix
	TTL    time.Duration

	mu   sync.Mutex
	at   time.Time
	list []netip.Prefix
}

// Subnets returns the cached ranges, refreshing them when older than TTL.
func (c *SubnetCache) Subnets() []netip.Prefix {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if c.Lookup != nil && time.Since(c.at) > ttl {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		// A Docker hiccup returns nothing: the last ranges known stay refused rather than none.
		if list := c.Lookup(ctx); len(list) > 0 {
			c.list = list
		}
		cancel()
		c.at = time.Now()
	}
	return c.list
}

// Refresh drops the cache, after the practice network was created or removed.
func (c *SubnetCache) Refresh() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}
