package conversation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A desktop that says its screen password (which its worker can read) in a reply, a note or a step leaves none of it
// in the chat (B7 interim, withhold.HoldAlways).
func TestAHeldScreenPasswordReachesNoChat(t *testing.T) {
	withhold.HoldAlways("screen:box", "Vnc-8kQz")
	t.Cleanup(func() { withhold.ReleaseAlways("screen:box") })
	s := newStore(t)
	s.AppendUserRequest("box", "t1", "check the screen")
	s.AppendActionStep("box", "typed Vnc-8kQz", Action{Kind: "type", Target: "Vnc-8kQz"})
	s.AppendAgentNote("box", "t1", KindEntry, "found VNC_PW=Vnc-8kQz")
	s.AppendAgentReply("box", "t1", KindEntry, "the password is Vnc-8kQz")
	if b, _ := json.Marshal(s.Snapshot("box")); strings.Contains(string(b), "Vnc-8kQz") {
		t.Fatalf("the chat holds the screen password: %s", b)
	}
}
