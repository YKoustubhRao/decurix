package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/evals"
	"brainiac/internal/store"
)

// trustKey makes the release key this test signs with the one the build
// trusts, and returns its private half.
func trustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := verifySignature
	verifySignature = func(data, sig []byte) (string, error) {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
		if err != nil || !ed25519.Verify(pub, data, raw) {
			return "", catalog.ErrBadSignature
		}
		return catalog.KeyID(pub), nil
	}
	t.Cleanup(func() { verifySignature = old })
	return priv
}

// staged runs a cycle that stages nova-2b in place of qwen3.5-2b as the brain.
func staged(t *testing.T) *fixture {
	t.Helper()
	f := setup(t, novaCard)
	f.opt.Apply = true
	res, err := Run(context.Background(), f.opt)
	if err != nil || res.Outcome != OutcomeStaged {
		t.Fatalf("%v %+v", err, res)
	}
	return f
}

func writeRelease(t *testing.T, st *store.Store, priv ed25519.PrivateKey) {
	t.Helper()
	manifest, sig, err := MakeRelease(st.Path(NextLock), priv, "test", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Path(ReleaseFile), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.SigPath(st.Path(ReleaseFile)), sig, 0o644); err != nil {
		t.Fatal(err)
	}
}

// parity records a parity eval of the staged lock in which nova-2b got
// newRight of 40 decided cases and qwen3.5-2b oldRight, in each field and
// changed role.
func parity(t *testing.T, st *store.Store, newRight, oldRight int, at time.Time) {
	t.Helper()
	var s Staged
	if err := st.ReadJSON(StagedFile, &s); err != nil {
		t.Fatal(err)
	}
	p := Parity{LockSHA: s.LockSHA, At: at, Fields: []string{"math", "general"}}
	// The brain is measured in the fields the box serves; the other roles
	// nova-2b takes (vision, ground) in their own.
	for _, f := range append(append([]string{}, p.Fields...), "vision", "ground") {
		p.Results.Set("nova-2b", f, evals.Score{N: 40, Correct: newRight, Decided: 40, DecidedCorrect: newRight, At: at})
		p.Results.Set("qwen3.5-2b", f, evals.Score{N: 40, Correct: oldRight, Decided: 40, DecidedCorrect: oldRight, At: at})
	}
	if err := WriteParity(st, p); err != nil {
		t.Fatal(err)
	}
}

// O-3.6: without a person, a staged plan is promoted only by a release
// signed with the release key that names its lock.
func TestAnUnsignedPromoteIsRefused(t *testing.T) {
	f := staged(t)
	_, _, err := Promote(f.st, now, PromoteOptions{})
	if !errors.Is(err, ErrNotReleased) {
		t.Fatalf("promoted with no release: %v", err)
	}
	// A release signed by a key the build does not trust is refused, and
	// not ignored even when a person promotes.
	trustKey(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	writeRelease(t, f.st, other)
	parity(t, f.st, 40, 20, now.Add(time.Hour))
	for _, byHand := range []bool{false, true} {
		if _, _, err := Promote(f.st, now, PromoteOptions{ByHand: byHand}); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("by hand %v: a release signed by an untrusted key promoted: %v", byHand, err)
		}
	}
	if brainOf(t, f.st, "lock.json") != "qwen3.5-2b" {
		t.Fatal("lock.json changed")
	}
}

// A signed release whose parity eval does not prove the new brain as good
// as the old one, in every field the box serves, is refused; so is one with
// no parity eval, a stale one, or one of another lock.
func TestASignedReleaseWithoutParityIsRefused(t *testing.T) {
	f := staged(t)
	priv := trustKey(t)
	writeRelease(t, f.st, priv)
	if _, _, err := Promote(f.st, now, PromoteOptions{}); err == nil || !strings.Contains(err.Error(), "parity") {
		t.Fatalf("promoted with no parity eval: %v", err)
	}
	parity(t, f.st, 34, 36, now.Add(time.Hour)) // 34/40 can claim about 74%, below 90%
	_, _, err := Promote(f.st, now, PromoteOptions{})
	if err == nil || !strings.Contains(err.Error(), "not proven") || !strings.Contains(err.Error(), "math") {
		t.Fatalf("promoted on a failed parity eval: %v", err)
	}
	parity(t, f.st, 40, 20, now.Add(-time.Hour)) // measured before the plan was staged
	if _, _, err := Promote(f.st, now, PromoteOptions{}); err == nil || !strings.Contains(err.Error(), "before the plan was staged") {
		t.Fatalf("promoted on a stale parity eval: %v", err)
	}
	var p Parity
	parity(t, f.st, 40, 20, now.Add(time.Hour))
	_ = f.st.ReadJSON(ParityFile, &p)
	p.LockSHA = strings.Repeat("0", 64)
	_ = WriteParity(f.st, p)
	if _, _, err := Promote(f.st, now, PromoteOptions{}); err == nil || !strings.Contains(err.Error(), "parity eval measured lock") {
		t.Fatalf("promoted on another lock's parity eval: %v", err)
	}
	// A release naming another lock is refused.
	parity(t, f.st, 40, 20, now.Add(time.Hour))
	good, _ := os.ReadFile(f.st.Path(NextLock))
	other := f.st.Path("other.json")
	_ = os.WriteFile(other, append(good, ' '), 0o644)
	manifest, sig, _ := MakeRelease(other, priv, "", now)
	_ = os.WriteFile(f.st.Path(ReleaseFile), manifest, 0o644)
	_ = os.WriteFile(catalog.SigPath(f.st.Path(ReleaseFile)), sig, 0o644)
	if _, _, err := Promote(f.st, now, PromoteOptions{}); err == nil || !strings.Contains(err.Error(), "names lock") {
		t.Fatalf("promoted on a release of another lock: %v", err)
	}
	if brainOf(t, f.st, "lock.json") != "qwen3.5-2b" {
		t.Fatal("lock.json changed")
	}
}

// A signed release of the staged lock whose parity eval passed promotes it,
// and the release and parity files are spent.
func TestASignedReleaseWithPassedParityPromotes(t *testing.T) {
	f := staged(t)
	writeRelease(t, f.st, trustKey(t))
	parity(t, f.st, 40, 30, now.Add(time.Hour)) // 40/40 can claim about 94%, above 75%
	lock, _, err := Promote(f.st, now, PromoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := lock.Plan.Pick(catalog.RoleBrain); b.Model != "nova-2b" || brainOf(t, f.st, "lock.json") != "nova-2b" {
		t.Fatalf("promoted %s", b.Model)
	}
	for _, name := range []string{ReleaseFile, ReleaseFile + ".sig", ParityFile, StagedFile} {
		if f.st.Exists(name) {
			t.Errorf("%s left behind", name)
		}
	}
}

// A file the release key signed that is not a release manifest (a catalog
// overlay) cannot stand in for one.
func TestOnlyAReleaseManifestCounts(t *testing.T) {
	f := staged(t)
	priv := trustKey(t)
	data := []byte(`{"version":"overlay","models":[]}` + "\n")
	_ = os.WriteFile(f.st.Path(ReleaseFile), data, 0o644)
	_ = os.WriteFile(catalog.SigPath(f.st.Path(ReleaseFile)), catalog.Sign(data, priv), 0o644)
	parity(t, f.st, 40, 20, now.Add(time.Hour))
	if _, _, err := Promote(f.st, now, PromoteOptions{}); err == nil || !strings.Contains(err.Error(), "not a release manifest") {
		t.Fatalf("a signed non-release promoted: %v", err)
	}
}
