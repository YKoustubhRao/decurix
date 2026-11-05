package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brainiac/internal/hub"
)

// A GitHub asset is downloaded only when something vouches for its bytes:
// the catalog's pin, else GitHub's digest; the two must agree.
func TestAnAssetNothingVouchesForIsRefused(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	cases := []struct {
		digest, pin, want string
		ok                bool
	}{
		{a, "", a, true},
		{"", a, a, true},
		{a, strings.ToUpper(a), a, true},
		{a, b, "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		got, err := assetSHA(hub.Asset{Name: "x.tar.gz", SHA256: c.digest}, c.pin)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("digest %q pin %q: %q %v", c.digest, c.pin, got, err)
		}
	}
}

// A llama.cpp build on disk is reused only when its files are the ones
// recorded at its install.
func TestALlamaBuildIsReusedOnlyWhenIntact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "b1-cpu")
	os.MkdirAll(filepath.Join(dir, "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "llama-server"), []byte("server"), 0o755)
	os.WriteFile(filepath.Join(dir, "lib", "libggml.so"), []byte("lib"), 0o644)
	if llamaIntact(dir) {
		t.Fatal("a build with no install record was trusted")
	}
	sums, err := treeSHAs(dir)
	if err != nil || len(sums) != 2 {
		t.Fatalf("%v %v", sums, err)
	}
	data := []byte(`{"llama-server":"` + sums["llama-server"] + `","lib/libggml.so":"` + sums["lib/libggml.so"] + `"}`)
	os.WriteFile(llamaManifest(dir), data, 0o644)
	if !llamaIntact(dir) {
		t.Fatal("an intact build was not trusted")
	}
	os.WriteFile(filepath.Join(dir, "lib", "libggml.so"), []byte("changed"), 0o644)
	if llamaIntact(dir) {
		t.Fatal("a changed library was trusted")
	}
	os.WriteFile(filepath.Join(dir, "lib", "libggml.so"), []byte("lib"), 0o644)
	os.WriteFile(filepath.Join(dir, "lib", "extra.so"), []byte("new"), 0o644)
	if llamaIntact(dir) {
		t.Fatal("a build with an added file was trusted")
	}
}
