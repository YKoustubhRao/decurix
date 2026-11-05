package querynotes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A note a desktop raises with its screen password in it (which its worker can read) is kept without it (B7 interim).
func TestAHeldScreenPasswordReachesNoQueryNote(t *testing.T) {
	withhold.HoldAlways("screen:box", "Vnc-8kQz")
	t.Cleanup(func() { withhold.ReleaseAlways("screen:box") })
	s := newTestStore(t)
	s.Add(QueryNote{ID: "n1", DesktopID: "box", Content: "is Vnc-8kQz the right one?", GateQuestion: "type Vnc-8kQz?",
		CreatedAt: time.Now(), Kind: "query"})
	if b, _ := json.Marshal(s.List()); strings.Contains(string(b), "Vnc-8kQz") || !strings.Contains(string(b), "n1") {
		t.Fatalf("notes: %s", b)
	}
}
