package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// githubAPI is the GitHub REST base URL (a variable so tests can point it at a fake).
var githubAPI = "https://api.github.com"

// Asset is one file attached to a GitHub release.
type Asset struct {
	Name   string
	URL    string // browser_download_url
	Size   int64
	SHA256 string // from the asset's "digest" ("sha256:<hex>"); "" when GitHub has none
}

// ReleaseAsset finds the asset with exactly this name in a release.
func ReleaseAsset(ctx context.Context, hc *http.Client, repo, tag, name string) (Asset, error) {
	return ReleaseAssetMatching(ctx, hc, repo, tag, func(n string) bool { return n == name })
}

// ReleaseAssetMatching finds the one asset of a release that match accepts;
// none is ErrNotFound and more than one is an error naming them.
func ReleaseAssetMatching(ctx context.Context, hc *http.Client, repo, tag string, match func(name string) bool) (Asset, error) {
	all, err := ReleaseAssets(ctx, hc, repo, tag)
	if err != nil {
		return Asset{}, err
	}
	var found []Asset
	for _, a := range all {
		if match(a.Name) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		return Asset{}, fmt.Errorf("hub: release %s %s has no matching asset: %w", repo, tag, ErrNotFound)
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, a := range found {
		names[i] = a.Name
	}
	return Asset{}, fmt.Errorf("hub: release %s %s: %d assets match (%s); be more specific", repo, tag, len(found), strings.Join(names, ", "))
}

// ReleaseAssets lists every asset of a release, each with GitHub's sha256
// digest when GitHub gives one (assets uploaded since mid-2025).
func ReleaseAssets(ctx context.Context, hc *http.Client, repo, tag string) ([]Asset, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !repoPartRe.MatchString(parts[0]) || !repoPartRe.MatchString(parts[1]) {
		return nil, fmt.Errorf("hub: %q is not a GitHub repo (owner/name)", repo)
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	u := strings.TrimRight(githubAPI, "/") + "/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", defaultAgent)
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := safeClient(hc).Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub: release %s %s: %w", repo, tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := statusError(resp)
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("hub: release %s %s: %w (%v)", repo, tag, ErrNotFound, err)
		}
		return nil, fmt.Errorf("hub: release %s %s: %w", repo, tag, err)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string  `json:"name"`
			Size   int64   `json:"size"`
			URL    string  `json:"browser_download_url"`
			Digest *string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("hub: release %s %s: %w", repo, tag, err)
	}
	out := make([]Asset, 0, len(rel.Assets))
	for _, a := range rel.Assets {
		asset := Asset{Name: a.Name, URL: a.URL, Size: a.Size}
		if a.Digest != nil {
			d := strings.ToLower(strings.TrimSpace(*a.Digest))
			if hexPart, ok := strings.CutPrefix(d, "sha256:"); ok && sha256Re.MatchString(hexPart) {
				asset.SHA256 = hexPart
			}
		}
		out = append(out, asset)
	}
	return out, nil
}
