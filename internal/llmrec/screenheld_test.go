package llmrec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A worker that read its screen's password (from /proc, its own uid's) and sent it in a model call cannot get it into
// the recorded call: a value held for its desktop's life (withhold.HoldAlways) reaches no blob and no row, as the
// call's tool output, its arguments and the answer (review RES-CP-1, B7 interim).
func TestAHeldScreenPasswordReachesNoRecordedCall(t *testing.T) {
	withhold.HoldAlways("screen:eve", "Vnc-8kQz")
	t.Cleanup(func() { withhold.ReleaseAlways("screen:eve") })
	s := testStore(t)
	body := chatBody(t, []any{
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{
			"name": "shell", "arguments": `{"cmd":"tr '\0' '\n' < /proc/77/environ"}`}}}},
		map[string]any{"role": "tool", "content": "HOME=/home/headless\nVNC_PW=Vnc-8kQz\nDISPLAY=:1"},
	}, nil)
	s.Record(context.Background(), Call{Provider: "azure", Desktop: "eve", Task: "t9", Purpose: "operator.turn",
		Request: body, Response: []byte(`{"choices":[{"message":{"content":"the screen password is Vnc-8kQz"}}]}`), Status: 200})
	files := blobFiles(t, s)
	if len(files) == 0 {
		t.Fatal("nothing stored")
	}
	for _, p := range files {
		if data, _ := os.ReadFile(p); strings.Contains(string(data), "Vnc-8kQz") {
			t.Fatalf("blob %s holds the screen password: %s", filepath.Base(p), data)
		}
	}
	rows, _ := s.Calls("t9")
	if b, _ := json.Marshal(rows); strings.Contains(string(b), "Vnc-8kQz") {
		t.Fatalf("a row holds the screen password: %s", b)
	}
}
