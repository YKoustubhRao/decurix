package memory

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A memory a desktop records with its screen password in it (which its worker can read) is kept without it (B7
// interim).
func TestAHeldScreenPasswordReachesNoMemory(t *testing.T) {
	withhold.HoldAlways("screen:box", "Vnc-8kQz")
	t.Cleanup(func() { withhold.ReleaseAlways("screen:box") })
	s := newStore(t)
	if _, err := s.Record("box", "screen Vnc-8kQz", "log in with Vnc-8kQz", "tip"); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(s.List("box")); strings.Contains(string(b), "Vnc-8kQz") || len(s.List("box")) != 1 {
		t.Fatalf("memory: %s", b)
	}
}
