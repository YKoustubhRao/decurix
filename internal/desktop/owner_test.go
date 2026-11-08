package desktop

// Tests for which control plane a container belongs to (OwnerLabel).

import (
	"bytes"
	"testing"
)

// A container is this control plane's only when its owner label names it; one naming another is foreign; one naming
// none (made before the label) is neither - listed, and left to no sweep.
func TestTheOwnerLabelSaysWhoseAContainerIs(t *testing.T) {
	ownerID = func() string { return "aaaaaaaaaaaa" }
	t.Cleanup(func() { ownerID = defaultOwnerID })
	mine, other, none := map[string]string{OwnerLabel: "aaaaaaaaaaaa"}, map[string]string{OwnerLabel: "bbbbbbbbbbbb"}, map[string]string{}
	if !OwnedHere(mine) || ForeignOwner(mine) {
		t.Fatal("this control plane's container is not its own")
	}
	if OwnedHere(other) || !ForeignOwner(other) {
		t.Fatal("another control plane's container is taken for this one's")
	}
	if OwnedHere(none) || ForeignOwner(none) {
		t.Fatal("a container naming no creator is owned or foreign")
	}
	if cfg, _, _ := buildContainerConfigs("img", "d1", "basic", "thebot", 1, 1024, nil, "vol", nil); cfg.Labels[OwnerLabel] != "aaaaaaaaaaaa" {
		t.Fatalf("a new desktop does not carry its creator: %v", cfg.Labels)
	}
}

// A desktop made before the owner label is this control plane's when the agent token it was created with is the one
// this control plane issues for it - another control plane's secret gives another token.
func TestALegacyDesktopIsAdoptedByItsAgentToken(t *testing.T) {
	mine := (&Manager{HostGUISecret: []byte("this plane's secret")}).AgentToken("erpbills")
	theirs := (&Manager{HostGUISecret: []byte("another plane's secret")}).AgentToken("erpbills")
	if mine == "" || mine == theirs {
		t.Fatal("tokens are not per control plane")
	}
	env := []string{"PATH=/usr/bin", AgentTokenEnv + "=" + mine}
	if !tokenIn(env, mine) || tokenIn(env, theirs) || tokenIn([]string{"PATH=/usr/bin"}, mine) {
		t.Fatal("adoption by agent token is wrong")
	}
}

// Two tars are joined entry by entry (joinTars): the worker's soul travels with its agent tree into practice.
func TestTwoTarsAreJoined(t *testing.T) {
	a := tarBody(map[string]string{"decurix_agent/core.py": "x"})
	b := tarBody(map[string]string{"soul": "binary"})
	joined, err := joinTars(a, b)
	if err != nil {
		t.Fatal(err)
	}
	sumJoined, _ := TarFingerprint(joined)
	sumA, _ := TarFingerprint(a)
	if sumJoined == sumA || !bytes.Contains(joined, []byte("soul")) || !bytes.Contains(joined, []byte("decurix_agent/core.py")) {
		t.Fatal("the joined tar does not hold both")
	}
}
