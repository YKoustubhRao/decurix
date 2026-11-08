package install

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"brainiac/internal/catalog"
	"brainiac/internal/hub"
)

// Pinner fills a catalog entry's missing pins from metadata alone: the
// Hugging Face commit "main" points at and the LFS sha256 the Hub lists for
// each file at that commit, and GitHub's sha256 digest for a release asset.
// It never downloads a model or an archive. A pin already there is kept,
// and must agree with what the Hub or GitHub says; it is never overwritten.
//
// What it writes vouches only for "this is what the uploader published when
// we looked". The trust comes from the person who runs it and signs the
// catalog (`brainiac catalog pin`, then `catalog sign`), or adopts the model
// (`brainiac scout adopt`).
type Pinner struct {
	HF *hub.Client
	// Assets lists a GitHub release's assets with their digests (default
	// hub.ReleaseAssets).
	Assets func(ctx context.Context, repo, tag string) ([]hub.Asset, error)
}

// NewPinner is a Pinner on the real Hub (HF_ENDPOINT, HF_TOKEN) and GitHub.
func NewPinner() Pinner {
	hc := &http.Client{}
	return Pinner{HF: hub.New(), Assets: func(ctx context.Context, repo, tag string) ([]hub.Asset, error) {
		return hub.ReleaseAssets(ctx, hc, repo, tag)
	}}
}

// ErrNoDigest is a GitHub asset GitHub gives no digest for: its sha256 can
// only be had by downloading it, which pinning does not do.
var ErrNoDigest = errors.New("GitHub gives no sha256 digest for it; pinning it needs its bytes (download it, check it by hand, and write its sha256)")

// Source fills what s lacks. Ollama, pip and system sources have no pins.
func (p Pinner) Source(ctx context.Context, s *catalog.Source) error {
	if s.Unpinned() == "" {
		return nil
	}
	switch s.Kind {
	case catalog.SourceHF:
		return p.hf(ctx, s)
	case catalog.SourceGitHub:
		assets, err := p.Assets(ctx, s.Repo, s.Tag)
		if err != nil {
			return err
		}
		for _, a := range assets {
			if a.Name != s.Asset {
				continue
			}
			if a.SHA256 == "" {
				return fmt.Errorf("%s %s: %w", s.Repo, s.Asset, ErrNoDigest)
			}
			s.SHA256 = a.SHA256
			return nil
		}
		return fmt.Errorf("%s %s has no asset %s: %w", s.Repo, s.Tag, s.Asset, hub.ErrNotFound)
	}
	return nil
}

// hf pins a Hugging Face source: its commit ("main" today when it has
// none), then each file's LFS sha256 at that commit.
func (p Pinner) hf(ctx context.Context, s *catalog.Source) error {
	rev := s.Revision
	if !catalog.IsCommit(rev) {
		commit, err := p.HF.Revision(ctx, s.Repo, rev)
		if err != nil {
			return err
		}
		rev = commit
	}
	files, err := p.HF.Files(ctx, s.Repo, rev)
	if err != nil {
		return err
	}
	byPath := map[string]hub.File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	pinned := make([]catalog.File, len(s.Files))
	copy(pinned, s.Files)
	for i, want := range pinned {
		f, ok := byPath[want.Path]
		switch {
		case !ok:
			return fmt.Errorf("%s has no file %s at %s", s.Repo, want.Path, rev[:12])
		case f.SHA256 == "":
			return fmt.Errorf("%s: %s is not an LFS file, so the Hub lists no sha256 for it", s.Repo, want.Path)
		case want.SHA256 != "" && want.SHA256 != f.SHA256:
			return fmt.Errorf("%s: %s is pinned to sha256 %s, but the Hub lists %s at %s", s.Repo, want.Path, want.SHA256, f.SHA256, rev[:12])
		}
		pinned[i].SHA256 = f.SHA256
	}
	s.Revision, s.Files = rev, pinned
	return nil
}

// Runtime fills the sha256 of every asset of the pinned llama.cpp release
// that GitHub gives a digest for, and returns the assets it could not pin.
func (p Pinner) Runtime(ctx context.Context, r *catalog.Runtime) (missing []string, err error) {
	if r.LlamaCppRepo == "" || r.LlamaCppTag == "" {
		return nil, nil
	}
	assets, err := p.Assets(ctx, r.LlamaCppRepo, r.LlamaCppTag)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for k, v := range r.LlamaCppSHA256 {
		sums[k] = v
	}
	for _, a := range assets {
		have := sums[a.Name]
		switch {
		case a.SHA256 == "":
			if have == "" {
				missing = append(missing, a.Name)
			}
		case have != "" && have != a.SHA256:
			return nil, fmt.Errorf("llama.cpp %s: %s is pinned to sha256 %s, but GitHub's digest is %s", r.LlamaCppTag, a.Name, have, a.SHA256)
		default:
			sums[a.Name] = a.SHA256
		}
	}
	r.LlamaCppSHA256 = sums
	return missing, nil
}

// Models pins every source of the models it can, and says per source what
// it could not pin (by "model (source)"). One failure does not stop the rest.
func (p Pinner) Models(ctx context.Context, models []catalog.Model) (pinned int, failed map[string]error) {
	failed = map[string]error{}
	for i := range models {
		for j := range models[i].Sources {
			s := &models[i].Sources[j]
			if s.Unpinned() == "" {
				continue
			}
			if err := p.Source(ctx, s); err != nil {
				failed[models[i].ID+" ("+s.Label()+")"] = err
				continue
			}
			if s.Unpinned() == "" && (s.Kind == catalog.SourceHF || s.Kind == catalog.SourceGitHub) {
				pinned++
			}
		}
	}
	return pinned, failed
}

// Overlay pins an overlay's models and llama.cpp build, failing on the
// first source it cannot pin: what is added to a catalog is installable.
func (p Pinner) Overlay(ctx context.Context, o *catalog.Overlay) error {
	if _, failed := p.Models(ctx, o.Models); len(failed) > 0 {
		var why []string
		for k, err := range failed {
			why = append(why, k+": "+err.Error())
		}
		sort.Strings(why)
		return fmt.Errorf("cannot pin %s", strings.Join(why, "; "))
	}
	if o.Runtime != nil {
		if _, err := p.Runtime(ctx, o.Runtime); err != nil {
			return err
		}
	}
	return nil
}
