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

// SubnetCache remembers the practice network's ranges for a short while, so the listener guard does not ask Docker
// on every request. Only the very first call waits for Docker; after that the ranges known are answered at once and
// asked for again in the background, one question at a time (review 2026-11-02: the guard asked Docker under its lock
// every second while no practice network existed, and every request on both listeners waited behind it).
type SubnetCache struct {
	// Lookup answers the practice network's ranges: none and no error when there is no practice network, an error
	// when Docker could not say.
	Lookup func(ctx context.Context) ([]netip.Prefix, error)
	TTL    time.Duration

	mu      sync.Mutex
	at      time.Time
	list    []netip.Prefix
	running chan struct{}
}

// Subnets returns the ranges known, asking Docker again in the background when they are older than TTL.
func (c *SubnetCache) Subnets() []netip.Prefix {
	c.mu.Lock()
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	// Knowing no range is not knowing there is none: a practice network made a moment ago is asked for again within
	// a second, not after the whole TTL (review 2026-11-02).
	if len(c.list) == 0 && ttl > time.Second {
		ttl = time.Second
	}
	var done chan struct{}
	if c.Lookup != nil && time.Since(c.at) > ttl {
		done = c.startLocked()
	}
	first, list := c.at.IsZero(), c.list
	c.mu.Unlock()
	if first && done != nil {
		<-done
		c.mu.Lock()
		list = c.list
		c.mu.Unlock()
	}
	return list
}

// startLocked asks Docker unless a question is out already, and returns the channel closed when it is answered. c.mu
// is held.
func (c *SubnetCache) startLocked() chan struct{} {
	if c.running != nil {
		return c.running
	}
	done := make(chan struct{})
	c.running = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		list, err := c.Lookup(ctx)
		cancel()
		c.mu.Lock()
		// A Docker hiccup (an error) keeps the last ranges known refused rather than none; a practice network that is
		// gone frees its ranges. Found live (2026-11-08): kept after the network was removed, a range Docker then gave
		// a new ordinary desktop stayed refused, and every callback of that desktop (its final reply, its questions)
		// was answered 403 until the control plane restarted.
		if err == nil {
			c.list = list
		}
		c.at, c.running = time.Now(), nil
		c.mu.Unlock()
		close(done)
	}()
	return done
}
