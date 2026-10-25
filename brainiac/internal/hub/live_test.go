package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveHub talks to the real Hugging Face Hub and GitHub. Opt-in:
//
//	BRAINIAC_LIVE=1 go test -run TestLiveHub -v ./internal/hub/
//
// It downloads only a README (a few KB) and never a model or a runtime.
func TestLiveHub(t *testing.T) {
	if os.Getenv("BRAINIAC_LIVE") != "1" {
		t.Skip("set BRAINIAC_LIVE=1 to talk to huggingface.co and api.github.com")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := New()
	const repo = "Qwen/Qwen3-VL-2B-Instruct-GGUF"

	commit, err := c.Revision(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s@main = %s", repo, commit)

	files, err := c.Files(ctx, repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	var readme File
	for _, f := range files {
		sha := f.SHA256
		if len(sha) > 12 {
			sha = sha[:12]
		}
		t.Logf("  %-45s %12d  %s", f.Path, f.Size, sha)
		if f.Path == "README.md" {
			readme = f
		}
		if strings.HasSuffix(f.Path, ".gguf") && f.SHA256 == "" {
			t.Errorf("%s has no sha256", f.Path)
		}
	}
	if readme.Path == "" {
		t.Fatal("no README.md listed")
	}
	dest := filepath.Join(t.TempDir(), "README.md")
	if err := c.Download(ctx, repo, commit, readme, dest, nil); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dest)
	if err != nil || st.Size() != readme.Size {
		t.Fatalf("README: %v, size %d want %d", err, st.Size(), readme.Size)
	}
	t.Logf("downloaded README.md: %d bytes", st.Size())

	asset, err := ReleaseAssetMatching(ctx, nil, "ggml-org/llama.cpp", "b11850", func(n string) bool {
		return strings.HasPrefix(n, "llama-") && strings.HasSuffix(n, "bin-win-cpu-x64.zip")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("llama.cpp b11850: %s  %d bytes  sha256:%s", asset.Name, asset.Size, asset.SHA256)
	if asset.SHA256 == "" {
		t.Error("GitHub gave no digest for the asset")
	}

	found, err := c.Search(ctx, SearchQuery{PipelineTag: "automatic-speech-recognition", Sort: "downloads", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range found {
		t.Logf("search: %-40s downloads %d  library %s  gated %v  modified %s", m.ID, m.Downloads, m.Library, m.Gated, m.LastModified.Format("2006-01-02"))
	}
	if len(found) == 0 {
		t.Error("search found nothing")
	}
}
