package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/flowera/internal/desktop"
)

// TestSendMessageError covers the mapping a human actually sees in the chat's
// error toast. The case that matters is ErrAgentUnreachable: a desktop whose
// agent has not bound :8081 yet used to answer every message with a blanket
// HTTP 500 reading "internal server error", which said nothing about what was
// wrong or whether retrying would help.
func TestSendMessageError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "agent not listening yet",
			err:        fmt.Errorf("send message to desktop box: %w: dial tcp: connection refused", desktop.ErrAgentUnreachable),
			wantStatus: http.StatusServiceUnavailable,
			wantSubstr: "not ready yet",
		},
		{
			name:       "desktop stopped",
			err:        fmt.Errorf("desktop box: %w", desktop.ErrNotRunning),
			wantStatus: http.StatusConflict,
			wantSubstr: "stopped",
		},
		{
			name:       "desktop gone",
			err:        fmt.Errorf("find desktop box: %w", desktop.ErrNotFound),
			wantStatus: http.StatusNotFound,
			wantSubstr: "no longer exists",
		},
		{
			name:       "anything else stays a 500",
			err:        fmt.Errorf("encode message: boom"),
			wantStatus: http.StatusInternalServerError,
			wantSubstr: "internal server error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()

			sendMessageError(rec, tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}

			if !strings.Contains(body["error"], tc.wantSubstr) {
				t.Errorf("error = %q, want it to contain %q", body["error"], tc.wantSubstr)
			}

			// jsonError's rule, which the new curated messages must not
			// quietly opt out of: the underlying error text never reaches
			// the client.
			if strings.Contains(body["error"], "dial tcp") || strings.Contains(body["error"], "boom") {
				t.Errorf("error %q leaks the internal error", body["error"])
			}
		})
	}
}
