package practice

import (
	"strings"
	"testing"
)

// A case's request is told to its gate as production tells one: the gate's copy keeps the site and withholds the
// password, the password is among the values, and the agent is told it is secret. A reply to a question asked for a
// password is withheld whole and given to the gate as a reply, never as a request copy.
func TestAPracticeNoticeIsProductions(t *testing.T) {
	n := RequestGateNotice("t1", "Sign in to records.example.test as mira, password: Plum7Orbit")
	if !n.SendCopy || !n.Withheld || strings.Contains(n.Copy, "Plum7Orbit") || !strings.Contains(n.Copy, "records.example.test") {
		t.Fatalf("request notice %+v", n)
	}
	found := false
	for _, v := range n.Values {
		found = found || strings.Contains(v, "Plum7Orbit")
	}
	if !found {
		t.Fatalf("the password is not among the values: %v", n.Values)
	}
	plain := RequestGateNotice("t1", "Export the March ledger")
	if plain.Withheld || len(plain.Values) != 0 || plain.Copy != "Export the March ledger" {
		t.Fatalf("an ordinary request was withheld: %+v", plain)
	}
	r := ReplyGateNotice("t1", "Fern42Kite", "What is the password for mira?", true)
	if r.SendCopy || r.Reply == nil || r.Reply.Kind != "query" || !r.Withheld || len(r.Values) == 0 {
		t.Fatalf("reply notice %+v", r)
	}
}
