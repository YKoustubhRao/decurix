package desktop

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestProxyTargetCacheAndInvalidation(t *testing.T) {
	m := &Manager{
		proxyTargets: make(map[string]proxyTargetCacheEntry),
	}

	testURL, _ := url.Parse("http://172.28.0.5:6901")
	sanitizedID := "testvm"
	cacheKey := sanitizedID + ":" + containerPort

	// Manually populate cache entry
	m.proxyTargetMu.Lock()
	m.proxyTargets[cacheKey] = proxyTargetCacheEntry{
		proxyURL: testURL,
		expires:  time.Now().Add(10 * time.Second),
	}
	m.proxyTargetMu.Unlock()

	// Resolve should return cached value without needing Docker client
	u, id, err := m.resolveProxyTarget(context.Background(), "testvm", containerPort)
	if err != nil {
		t.Fatalf("unexpected error from cached resolveProxyTarget: %v", err)
	}
	if id != sanitizedID {
		t.Fatalf("expected id %s, got %s", sanitizedID, id)
	}
	if u.String() != testURL.String() {
		t.Fatalf("expected URL %s, got %s", testURL, u)
	}

	// Invalidate target
	m.InvalidateProxyTarget("testvm")

	m.proxyTargetMu.RLock()
	_, found := m.proxyTargets[cacheKey]
	m.proxyTargetMu.RUnlock()
	if found {
		t.Fatalf("expected cache key %s to be invalidated, but it was found", cacheKey)
	}
}
