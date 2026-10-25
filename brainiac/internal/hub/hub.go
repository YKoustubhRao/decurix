// Package hub fetches model files from Hugging Face and runtime binaries from
// GitHub releases: pinned to exact commits, resumable after a broken
// connection, and checked by sha256 before a file is put where a runtime
// would load it. Credentials never follow a redirect to another host.
//
// Depends on nothing internal.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors, matched with errors.Is.
var (
	ErrGated    = errors.New("access refused: the model may be gated or private - accept its licence on the model page and set HF_TOKEN")
	ErrNotFound = errors.New("not found")
	ErrChecksum = errors.New("checksum mismatch")
)

const (
	defaultEndpoint = "https://huggingface.co"
	defaultAgent    = "brainiac-hub/1"
	apiTimeout      = 60 * time.Second
	maxPages        = 1000
)

// StatusError is an HTTP answer other than success. It unwraps to ErrGated or
// ErrNotFound when the status means one of those.
type StatusError struct {
	URL     string // without its query string: signed CDN URLs carry signatures there
	Status  int
	Code    string // Hugging Face's X-Error-Code, or "RateLimited"
	Message string
}

// Error describes the failed request, with advice when there is some.
func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s: HTTP %d", e.URL, e.Status)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	switch e.Unwrap() {
	case ErrGated:
		msg += " - " + ErrGated.Error()
	case ErrNotFound:
		if e.Status == http.StatusUnauthorized {
			msg += " - not found (if the repo is private, set HF_TOKEN)"
		}
	}
	return msg
}

// Unwrap maps the status to a sentinel error.
func (e *StatusError) Unwrap() error {
	switch e.Code {
	case "RepoNotFound", "EntryNotFound", "RevisionNotFound":
		return ErrNotFound
	case "GatedRepo":
		return ErrGated
	case "RateLimited":
		return nil
	}
	switch e.Status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrGated
	}
	return nil
}

// statusError reads a failed response into a StatusError.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	e := &StatusError{URL: withoutQuery(resp.Request.URL), Status: resp.StatusCode, Code: resp.Header.Get("X-Error-Code")}
	if m := resp.Header.Get("X-Error-Message"); m != "" {
		e.Message = m
	} else {
		var j struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &j) == nil && (j.Error != "" || j.Message != "") {
			e.Message = j.Error
			if e.Message == "" {
				e.Message = j.Message
			}
		} else {
			e.Message = strings.TrimSpace(string(body))
		}
	}
	if len(e.Message) > 300 {
		e.Message = e.Message[:300] + "..."
	}
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0" {
		e.Code = "RateLimited"
		e.Message = "rate limit reached; wait, or set a token (GITHUB_TOKEN / HF_TOKEN)"
	}
	return e
}

// withoutQuery is a URL with its query and fragment removed.
func withoutQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.RawQuery, c.Fragment, c.User = "", "", nil
	return c.String()
}

// Client talks to the Hugging Face Hub.
type Client struct {
	Endpoint  string
	Token     string
	HTTP      *http.Client
	UserAgent string
}

// New returns a Client set up from HF_ENDPOINT (a mirror) and HF_TOKEN.
func New() *Client {
	c := &Client{Endpoint: strings.TrimRight(os.Getenv("HF_ENDPOINT"), "/"), Token: strings.TrimSpace(os.Getenv("HF_TOKEN"))}
	if c.Endpoint == "" {
		c.Endpoint = defaultEndpoint
	}
	return c
}

// endpoint is the Hub base URL without a trailing slash.
func (c *Client) endpoint() string {
	if c.Endpoint == "" {
		return defaultEndpoint
	}
	return strings.TrimRight(c.Endpoint, "/")
}

// agent is the User-Agent sent with every request.
func (c *Client) agent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return defaultAgent
}

// headers are the request headers for the Hub, the token included.
func (c *Client) headers() map[string]string {
	h := map[string]string{"User-Agent": c.agent()}
	if c.Token != "" {
		h["Authorization"] = "Bearer " + c.Token
	}
	return h
}

// getJSON GETs an API URL and decodes its JSON body into out.
func (c *Client) getJSON(ctx context.Context, rawURL string, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := safeClient(c.HTTP).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", withoutQuery(resp.Request.URL), err)
	}
	return resp.Header, nil
}

var (
	repoPartRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	commitRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// repoPath checks a repo id ("owner/name" or a legacy "name") and escapes it
// for a URL path.
func repoPath(repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) > 2 {
		return "", fmt.Errorf("hub: %q is not a repo id (owner/name)", repo)
	}
	for _, p := range parts {
		if !repoPartRe.MatchString(p) || strings.Contains(p, "..") {
			return "", fmt.Errorf("hub: %q is not a repo id (owner/name)", repo)
		}
	}
	return repo, nil
}

// escapePath escapes each segment of a file path inside a repo.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Revision resolves a branch, tag or commit of a model repo to its 40-hex
// commit, which is what a lock file pins.
func (c *Client) Revision(ctx context.Context, repo, rev string) (string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	if rev == "" {
		rev = "main"
	}
	var out struct {
		SHA string `json:"sha"`
	}
	u := c.endpoint() + "/api/models/" + rp + "/revision/" + url.PathEscape(rev)
	if _, err := c.getJSON(ctx, u, &out); err != nil {
		return "", fmt.Errorf("hub: revision %s@%s: %w", repo, rev, err)
	}
	sha := strings.ToLower(out.SHA)
	if !commitRe.MatchString(sha) {
		return "", fmt.Errorf("hub: revision %s@%s: the Hub answered %q, not a commit", repo, rev, out.SHA)
	}
	return sha, nil
}

// File is one file of a model repo at a commit.
type File struct {
	Path   string
	Size   int64
	SHA256 string // the LFS object id (sha256 of the content); "" for a small non-LFS file
	GitOID string // git blob id
}

// treeEntry is one element of the Hub's tree listing.
type treeEntry struct {
	Type string `json:"type"`
	Oid  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	LFS  *struct {
		Oid  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// Files lists every file of a repo at a commit, following the listing's pages.
func (c *Client) Files(ctx context.Context, repo, commit string) ([]File, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(c.endpoint())
	if err != nil {
		return nil, fmt.Errorf("hub: endpoint %q: %w", c.endpoint(), err)
	}
	next := c.endpoint() + "/api/models/" + rp + "/tree/" + url.PathEscape(commit) + "?recursive=true"
	var files []File
	seen := map[string]bool{}
	for page := 0; next != ""; page++ {
		if page >= maxPages || seen[next] {
			return nil, fmt.Errorf("hub: files of %s: the listing does not end", repo)
		}
		seen[next] = true
		var entries []treeEntry
		hdr, err := c.getJSON(ctx, next, &entries)
		if err != nil {
			return nil, fmt.Errorf("hub: files of %s@%s: %w", repo, commit, err)
		}
		for _, e := range entries {
			if e.Type != "file" {
				continue
			}
			f := File{Path: e.Path, Size: e.Size, GitOID: e.Oid}
			if e.LFS != nil {
				f.SHA256 = strings.ToLower(e.LFS.Oid)
				if e.LFS.Size > 0 {
					f.Size = e.LFS.Size
				}
			}
			files = append(files, f)
		}
		next, err = nextLink(hdr.Get("Link"), next, base.Host)
		if err != nil {
			return nil, fmt.Errorf("hub: files of %s: %w", repo, err)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// nextLink is the rel="next" URL of a Link header, resolved against the
// current page; it must stay on the Hub's own host, since the token goes with it.
func nextLink(header, current, host string) (string, error) {
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		isNext := false
		for _, s := range segs[1:] {
			s = strings.TrimSpace(s)
			if s == `rel="next"` || s == "rel=next" {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		cur, err := url.Parse(current)
		if err != nil {
			return "", err
		}
		u, err := cur.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("bad next link %q: %w", raw, err)
		}
		if u.Host != host {
			return "", fmt.Errorf("next page is on another host (%s)", u.Host)
		}
		return u.String(), nil
	}
	return "", nil
}

// Download fetches one repo file at a commit into dest, resuming a .part left
// by an earlier try and checking the sha256 (or the size, for a small file).
func (c *Client) Download(ctx context.Context, repo, commit string, f File, dest string, progress func(done, total int64)) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	u := c.endpoint() + "/" + rp + "/resolve/" + url.PathEscape(commit) + "/" + escapePath(f.Path)
	if err := DownloadURL(ctx, c.HTTP, u, dest, f.Size, f.SHA256, c.headers(), progress); err != nil {
		return fmt.Errorf("hub: %s/%s: %w", repo, f.Path, err)
	}
	return nil
}

// ModelInfo is one model in a Hub search.
type ModelInfo struct {
	ID           string
	Downloads    int
	Likes        int
	PipelineTag  string
	Library      string
	Tags         []string
	Gated        bool
	LastModified time.Time
}

// SearchQuery filters a Hub search.
type SearchQuery struct {
	Search      string
	Author      string
	Filter      []string // tags, e.g. "gguf", "license:apache-2.0"
	PipelineTag string   // e.g. "automatic-speech-recognition"
	Sort        string   // downloads | likes | trending_score | lastModified | createdAt
	Limit       int
}

// sortKeys maps accepted Sort values to the Hub's own names.
var sortKeys = map[string]string{
	"downloads": "downloads", "likes": "likes",
	"trending_score": "trendingScore", "trendingScore": "trendingScore",
	"lastModified": "lastModified", "last_modified": "lastModified",
	"createdAt": "createdAt", "created_at": "createdAt",
}

// Search lists models matching q, most first by q.Sort. Discovery only: what
// it finds is a candidate to test, never a model to use.
func (c *Client) Search(ctx context.Context, q SearchQuery) ([]ModelInfo, error) {
	v := url.Values{}
	if q.Search != "" {
		v.Set("search", q.Search)
	}
	if q.Author != "" {
		v.Set("author", q.Author)
	}
	for _, f := range q.Filter {
		v.Add("filter", f)
	}
	if q.PipelineTag != "" {
		v.Set("pipeline_tag", q.PipelineTag)
	}
	sortBy := q.Sort
	if sortBy == "" {
		sortBy = "downloads"
	}
	key, ok := sortKeys[sortBy]
	if !ok {
		return nil, fmt.Errorf("hub: unknown sort %q", q.Sort)
	}
	v.Set("sort", key)
	v.Set("direction", "-1")
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	v.Set("limit", strconv.Itoa(limit))
	for _, e := range []string{"downloads", "likes", "pipeline_tag", "library_name", "tags", "gated", "lastModified"} {
		v.Add("expand", e)
	}

	var raw []struct {
		ID           string          `json:"id"`
		ModelID      string          `json:"modelId"`
		Downloads    int             `json:"downloads"`
		Likes        int             `json:"likes"`
		PipelineTag  string          `json:"pipeline_tag"`
		Library      string          `json:"library_name"`
		Tags         []string        `json:"tags"`
		Gated        json.RawMessage `json:"gated"`
		LastModified time.Time       `json:"lastModified"`
	}
	if _, err := c.getJSON(ctx, c.endpoint()+"/api/models?"+v.Encode(), &raw); err != nil {
		return nil, fmt.Errorf("hub: search: %w", err)
	}
	out := make([]ModelInfo, 0, len(raw))
	for _, r := range raw {
		id := r.ID
		if id == "" {
			id = r.ModelID
		}
		g := strings.TrimSpace(string(r.Gated))
		out = append(out, ModelInfo{
			ID: id, Downloads: r.Downloads, Likes: r.Likes, PipelineTag: r.PipelineTag, Library: r.Library,
			Tags: r.Tags, LastModified: r.LastModified,
			Gated: g != "" && g != "false" && g != "null",
		})
	}
	return out, nil
}
