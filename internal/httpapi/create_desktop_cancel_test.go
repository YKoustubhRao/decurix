package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateDesktopHandlerCanceledContext(t *testing.T) {
	mgr := fakeDesktopManager(t)
	srv := &Server{
		Desktops: mgr,
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Create request with already canceled context (simulating client abort)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	body := strings.NewReader("name=test-cancel&profile=basic&harness=ollama&cpus=2&memory_mb=2048")
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", body).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	start := time.Now()
	mux.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	// Should abort immediately without waiting or hanging
	if elapsed > 2*time.Second {
		t.Fatalf("expected instant cancellation, took %v", elapsed)
	}
}
