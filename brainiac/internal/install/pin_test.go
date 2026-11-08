package install

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/hub"
)

// Pinning reads metadata only: the commit "main" points at, the LFS sha256
// of each file at it, GitHub's digest of each asset. It keeps a pin that is
// there, refuses one the Hub contradicts, and never downloads a file.
func TestPinnerFillsPinsFromMetadataAlone(t *testing.T) {
	commit, sum := strings.Repeat("c", 40), strings.Repeat("d", 64)
	var downloads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			fmt.Fprintf(w, `{"sha":%q}`, commit)
		case strings.Contains(r.URL.Path, "/tree/"+commit):
			fmt.Fprintf(w, `[{"type":"file","path":"m.gguf","size":5,"lfs":{"oid":%q,"size":5}},{"type":"file","path":"small.txt","size":3,"oid":"ab"}]`, sum)
		case strings.Contains(r.URL.Path, "/resolve/"):
			downloads++
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := Pinner{HF: &hub.Client{Endpoint: srv.URL}, Assets: func(_ context.Context, repo, tag string) ([]hub.Asset, error) {
		return []hub.Asset{{Name: "with.tar.bz2", SHA256: sum}, {Name: "without.tar.bz2"}}, nil
	}}
	ctx := context.Background()

	hf := catalog.Source{Kind: catalog.SourceHF, Repo: "lab/M", Files: []catalog.File{{Path: "m.gguf", Role: "model"}}}
	if err := p.Source(ctx, &hf); err != nil || hf.Revision != commit || hf.Files[0].SHA256 != sum || hf.Unpinned() != "" {
		t.Fatalf("%+v %v", hf, err)
	}
	wrong := catalog.Source{Kind: catalog.SourceHF, Repo: "lab/M", Files: []catalog.File{{Path: "m.gguf", Role: "model", SHA256: strings.Repeat("0", 64)}}}
	if err := p.Source(ctx, &wrong); err == nil || wrong.Revision != "" {
		t.Fatalf("a pin the Hub contradicts was overwritten: %+v %v", wrong, err)
	}
	small := catalog.Source{Kind: catalog.SourceHF, Repo: "lab/M", Files: []catalog.File{{Path: "small.txt", Role: "tokens"}}}
	if err := p.Source(ctx, &small); err == nil {
		t.Fatal("a file with no LFS sha256 was pinned")
	}

	gh := catalog.Source{Kind: catalog.SourceGitHub, Repo: "k/s", Tag: "t", Asset: "with.tar.bz2"}
	if err := p.Source(ctx, &gh); err != nil || gh.SHA256 != sum {
		t.Fatalf("%+v %v", gh, err)
	}
	none := catalog.Source{Kind: catalog.SourceGitHub, Repo: "k/s", Tag: "t", Asset: "without.tar.bz2"}
	if err := p.Source(ctx, &none); !errors.Is(err, ErrNoDigest) || none.SHA256 != "" {
		t.Fatalf("an asset with no digest: %+v %v", none, err)
	}

	rt := catalog.Runtime{LlamaCppRepo: "ggml-org/llama.cpp", LlamaCppTag: "b1"}
	missing, err := p.Runtime(ctx, &rt)
	if err != nil || rt.LlamaCppSHA256["with.tar.bz2"] != sum || len(missing) != 1 || missing[0] != "without.tar.bz2" {
		t.Fatalf("%+v %v %v", rt, missing, err)
	}
	if downloads != 0 {
		t.Fatalf("pinning downloaded %d files", downloads)
	}
}
