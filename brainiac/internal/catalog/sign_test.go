package catalog

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// devKey is a key pair made for one test alone, trusted only while it runs:
// no private key is ever written to the repository.
func devKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := extraKeys
	extraKeys = append(append([]ed25519.PublicKey(nil), old...), pub)
	t.Cleanup(func() { extraKeys = old })
	return priv
}

func TestASignedCatalogVerifiesAndAChangedOneDoesNot(t *testing.T) {
	priv := devKey(t)
	data := []byte(`{"updated":"2026-11-08","models":[]}` + "\n")
	sig := Sign(data, priv)
	if id, err := Verify(data, sig); err != nil || id != KeyID(priv.Public().(ed25519.PublicKey)) {
		t.Fatalf("a good signature: %q %v", id, err)
	}
	changed := bytes.Replace(data, []byte("11-08"), []byte("11-09"), 1)
	if _, err := Verify(changed, sig); err == nil {
		t.Fatal("a changed file verified")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(data, Sign(data, other)); err == nil {
		t.Fatal("a key this build does not trust verified")
	}
	if _, err := Verify(data, []byte("not base64!")); err == nil {
		t.Fatal("a malformed signature verified")
	}
}

func TestABuildWithoutAKeyTrustsNothing(t *testing.T) {
	if releaseKeyHex != "" {
		t.Skip("this build carries a release key")
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	data := []byte("{}\n")
	if _, err := Verify(data, Sign(data, priv)); err == nil || !strings.Contains(err.Error(), "trusts no signing key") {
		t.Fatalf("%v", err)
	}
}

// The private key file `catalog keygen` writes is read back by `sign`.
func TestReadPrivateKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := filepath.Join(t.TempDir(), "k")
	os.WriteFile(p, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600)
	got, err := ReadPrivateKey(p)
	if err != nil || !got.Equal(priv) {
		t.Fatalf("%v", err)
	}
	os.WriteFile(p, []byte("abcd"), 0o600)
	if _, err := ReadPrivateKey(p); err == nil {
		t.Fatal("a short key was read")
	}
}

// An overlay is used only when its signature verifies; without one it is
// the operator's own, used off an appliance and never on one (O-3.3).
func TestAnOverlayIsUsedOnlyWhenItsSignatureVerifies(t *testing.T) {
	priv := devKey(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.local.json")
	base, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	m, _ := base.Model("qwen3.5-2b")
	m.ID, m.Title = "signed-model", "a model a signed overlay adds"
	if err := SaveOverlay(path, Overlay{Updated: "2026-11-08", Models: []Model{m}}); err != nil {
		t.Fatal(err)
	}
	has := func(signedOnly bool) (bool, string) {
		t.Helper()
		c, note, err := WithOverlay(path, signedOnly)
		if err != nil {
			t.Fatal(err)
		}
		_, ok := c.Model("signed-model")
		return ok, note
	}
	// Unsigned: the operator's own.
	if ok, note := has(false); !ok || note != "" {
		t.Fatalf("an unsigned overlay off an appliance: used %v, %q", ok, note)
	}
	if ok, note := has(true); ok || !strings.Contains(note, "not signed") {
		t.Fatalf("an unsigned overlay on an appliance: used %v, %q", ok, note)
	}
	// Signed.
	data, _ := os.ReadFile(path)
	os.WriteFile(SigPath(path), Sign(data, priv), 0o644)
	for _, appliance := range []bool{false, true} {
		if ok, note := has(appliance); !ok || note != "" {
			t.Fatalf("a signed overlay (appliance %v): used %v, %q", appliance, ok, note)
		}
	}
	// Changed after signing: used nowhere.
	os.WriteFile(path, bytes.Replace(data, []byte("2026-11-08"), []byte("2026-11-09"), 1), 0o644)
	for _, appliance := range []bool{false, true} {
		if ok, note := has(appliance); ok || !strings.Contains(note, "not used") {
			t.Fatalf("a changed signed overlay (appliance %v): used %v, %q", appliance, ok, note)
		}
	}
	// The operator writing it makes it theirs again: the stale signature goes.
	if err := SaveOverlay(path, Overlay{Updated: "2026-11-10", Models: []Model{m}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SigPath(path)); !os.IsNotExist(err) {
		t.Fatalf("the old signature stayed beside a rewritten overlay: %v", err)
	}
}

// stripPins is the shipped catalog as it was before it was pinned.
func stripPins(t *testing.T) []byte {
	t.Helper()
	root, err := parseOrdered(embedded)
	if err != nil {
		t.Fatal(err)
	}
	var strip func(n *jnode)
	strip = func(n *jnode) {
		if n.kind == '{' {
			for i := 0; i < len(n.keys); i++ {
				if k := n.keys[i]; k == "revision" || k == "sha256" || k == "llamacpp_sha256" {
					n.keys = append(n.keys[:i], n.keys[i+1:]...)
					n.vals = append(n.vals[:i], n.vals[i+1:]...)
					i--
				}
			}
		}
		for _, v := range n.vals {
			strip(v)
		}
	}
	strip(root)
	return root.bytes()
}

// The shipped catalog file is laid out as WritePins writes, so pins are
// added as a plain diff; with nothing to pin it is unchanged.
func TestWritePinsChangesOnlyThePins(t *testing.T) {
	if got, err := WritePins(embedded, nil, nil); err != nil || !bytes.Equal(got, normaliseEOL(embedded)) {
		t.Fatalf("a no-op rewrite changed the catalog: %v", err)
	}
	unpinned := stripPins(t)
	c, err := Parse(unpinned)
	if err != nil {
		t.Fatal(err)
	}
	commit, sum := strings.Repeat("a", 40), strings.Repeat("b", 64)
	var hfID, ghID string
	for i := range c.Models {
		for j := range c.Models[i].Sources {
			s := &c.Models[i].Sources[j]
			switch {
			case s.Kind == SourceHF && hfID == "":
				hfID = c.Models[i].ID
				s.Revision = commit
				for k := range s.Files {
					s.Files[k].SHA256 = sum
				}
			case s.Kind == SourceGitHub && ghID == "":
				ghID = c.Models[i].ID
				s.SHA256 = sum
			}
		}
	}
	rt := c.Runtime
	rt.LlamaCppSHA256 = map[string]string{"llama-x.zip": sum}
	out, err := WritePins(unpinned, c.Models, &rt)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{hfID, ghID} {
		m, _ := after.Model(id)
		if why := m.Sources[0].Unpinned(); why != "" && m.Sources[0].Kind != SourceOllama {
			t.Errorf("%s still unpinned: %s", id, why)
		}
	}
	if after.Runtime.LlamaCppSHA256["llama-x.zip"] != sum {
		t.Error("the runtime pin was not written")
	}
	// Only lines were added: every original line is still there, in order.
	orig := strings.Split(string(unpinned), "\n")
	got := strings.Split(string(out), "\n")
	i := 0
	for _, line := range got {
		if i < len(orig) && strings.TrimRight(line, ",") == strings.TrimRight(orig[i], ",") {
			i++
		}
	}
	if i != len(orig) {
		t.Fatalf("an original line moved or changed near %q", orig[i])
	}
	if !strings.Contains(string(out), `"revision": "`+commit+`"`) {
		t.Fatal("no revision line")
	}
}

func TestUnpinnedSaysWhatIsMissing(t *testing.T) {
	hf := Source{Kind: SourceHF, Runtime: RuntimeLlamaCpp, Repo: "a/b", Files: []File{{Path: "m.gguf", Role: "model"}}, SizeGB: 1}
	if why := hf.Unpinned(); !strings.Contains(why, "no revision") || !strings.Contains(why, "m.gguf") {
		t.Fatalf("%q", why)
	}
	hf.Revision, hf.Files[0].SHA256 = strings.Repeat("0", 40), strings.Repeat("1", 64)
	if why := hf.Unpinned(); why != "" {
		t.Fatalf("%q", why)
	}
	if why := (Source{Kind: SourceOllama, Tag: "x"}).Unpinned(); why != "" {
		t.Fatalf("an Ollama tag: %q", why)
	}
	// A pin that is there must be well formed.
	hf.Revision = "main"
	if err := hf.validate(); err == nil {
		t.Fatal(`revision "main" validated`)
	}
}
