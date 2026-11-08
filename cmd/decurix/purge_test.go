// purge_test.go checks `decurix --purge-secret`: the value goes from stdin into the request body only, a short one is
// refused before anything is sent, and the value is never printed.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The value read from stdin is posted in the body (never the URL) with the reason, and the counts are printed.
func TestPurgeSecretPostsTheValueFromStdin(t *testing.T) {
	var got map[string]string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.String()
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"counts":{"blobs":2,"llm_calls":1},"judgement":"j_0123456789abcdef","problems":[]}`))
	}))
	defer srv.Close()
	var out, errs strings.Builder
	code := runPurgeSecret(srv.URL, "typed into chat", strings.NewReader("  saltmarsh-ledger-77\n"), &out, &errs)
	if code != 0 || got["value"] != "saltmarsh-ledger-77" || got["reason"] != "typed into chat" || path != "/api/privacy/purge" {
		t.Fatalf("code %d, body %v, path %s, stderr %s", code, got, path, errs.String())
	}
	if strings.Contains(out.String()+errs.String(), "saltmarsh") || !strings.Contains(out.String(), "j_0123456789abcdef") {
		t.Errorf("output: %s / %s", out.String(), errs.String())
	}
}

// A value of five characters, or no reason, is refused before anything is sent.
func TestPurgeSecretRefusesBeforeSending(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent = true }))
	defer srv.Close()
	var out, errs strings.Builder
	if code := runPurgeSecret(srv.URL, "leaked", strings.NewReader("abcde\n"), &out, &errs); code != 2 {
		t.Errorf("short value: code %d", code)
	}
	if code := runPurgeSecret(srv.URL, "", strings.NewReader("saltmarsh-ledger-77\n"), &out, &errs); code != 2 {
		t.Errorf("no reason: code %d", code)
	}
	if sent || strings.Contains(errs.String(), "abcde") {
		t.Errorf("sent %v, stderr %s", sent, errs.String())
	}
}
