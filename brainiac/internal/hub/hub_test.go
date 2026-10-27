package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// fakeHub is a stand-in for the Hugging Face Hub.
type fakeHub struct {
	mu          sync.Mutex
	files       map[string][]byte
	ignoreRange bool
	stallFirst  bool
	resolveHits int
	ranges      []string
	auth        []string
}

func (f *fakeHub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/org/repo/revision/main", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"sha": testCommit})
	})
	mux.HandleFunc("/api/models/org/gated/revision/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Error-Code", "GatedRepo")
		http.Error(w, `{"error":"Access to model org/gated is restricted"}`, http.StatusForbidden)
	})
	mux.HandleFunc("/api/models/org/missing/revision/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Error-Code", "RepoNotFound")
		http.Error(w, `{"error":"Repository not found"}`, http.StatusUnauthorized)
	})
	mux.HandleFunc("/api/models/org/private/revision/main", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
	mux.HandleFunc("/api/models/org/repo/tree/"+testCommit, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "true" {
			t.Errorf("tree asked without recursive=true: %s", r.URL)
		}
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", `<`+"/api/models/org/repo/tree/"+testCommit+`?recursive=true&cursor=p2>; rel="next"`)
			fmt.Fprint(w, `[{"type":"directory","oid":"d1","size":0,"path":"sub"},
				{"type":"file","oid":"g1","size":12,"path":"README.md"},
				{"type":"file","oid":"g2","size":130,"path":"sub/model.gguf","lfs":{"oid":"ABCDEF0000000000000000000000000000000000000000000000000000000000","size":999999,"pointerSize":130}}]`)
			return
		}
		fmt.Fprint(w, `[{"type":"file","oid":"g3","size":5,"path":"config.json"}]`)
	})
	mux.HandleFunc("/org/repo/resolve/"+testCommit+"/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/org/repo/resolve/"+testCommit+"/")
		f.mu.Lock()
		f.resolveHits++
		hit := f.resolveHits
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		data, ok := f.files[name]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if f.stallFirst && hit == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data[:len(data)/2])
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		if f.ignoreRange {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.Write(data)
			return
		}
		http.ServeContent(w, r, name, time.Unix(1700000000, 0), bytes.NewReader(data))
	})
	return mux
}

// testData is n pseudo-random bytes and their sha256.
func testData(n int) ([]byte, string) {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:])
}

func newFake(t *testing.T, f *fakeHub) (*Client, *httptest.Server) {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return &Client{Endpoint: srv.URL, HTTP: srv.Client()}, srv
}

func TestRevisionResolvesToCommit(t *testing.T) {
	c, _ := newFake(t, &fakeHub{})
	commit, err := c.Revision(context.Background(), "org/repo", "")
	if err != nil || commit != testCommit {
		t.Fatalf("Revision = %q, %v", commit, err)
	}
}

func TestRevisionErrorsAreTyped(t *testing.T) {
	c, _ := newFake(t, &fakeHub{})
	ctx := context.Background()
	if _, err := c.Revision(ctx, "org/gated", "main"); !errors.Is(err, ErrGated) {
		t.Errorf("gated repo: %v, want ErrGated", err)
	} else if !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("gated error gives no advice: %v", err)
	}
	if _, err := c.Revision(ctx, "org/missing", "main"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing repo (401 RepoNotFound): %v, want ErrNotFound", err)
	}
	if _, err := c.Revision(ctx, "org/private", "main"); !errors.Is(err, ErrGated) {
		t.Errorf("plain 401: %v, want ErrGated", err)
	}
	if _, err := c.Revision(ctx, "org/nothing", "main"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v, want ErrNotFound", err)
	}
	for _, bad := range []string{"../etc", "a/b/c", "", "org/..", "org/re po"} {
		if _, err := c.Revision(ctx, bad, "main"); err == nil {
			t.Errorf("repo id %q accepted", bad)
		}
	}
}

func TestFilesFollowsPagesAndReadsLFS(t *testing.T) {
	c, _ := newFake(t, &fakeHub{})
	files, err := c.Files(context.Background(), "org/repo", testCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3 (directories skipped, both pages): %+v", len(files), files)
	}
	want := []string{"README.md", "config.json", "sub/model.gguf"}
	for i, f := range files {
		if f.Path != want[i] {
			t.Errorf("file %d = %s, want %s", i, f.Path, want[i])
		}
	}
	g := files[2]
	if g.Size != 999999 || g.SHA256 != "abcdef0000000000000000000000000000000000000000000000000000000000" || g.GitOID != "g2" {
		t.Errorf("LFS file read as %+v", g)
	}
	if files[0].SHA256 != "" || files[0].Size != 12 {
		t.Errorf("small file read as %+v", files[0])
	}
}

func TestFilesRefusesNextPageOnAnotherHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://evil.example/api/x?cursor=2>; rel="next"`)
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, HTTP: srv.Client(), Token: "secret"}
	if _, err := c.Files(context.Background(), "org/repo", testCommit); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("next page on another host: %v", err)
	}
}

func TestDownloadFresh(t *testing.T) {
	data, sum := testData(3<<20 + 17)
	c, _ := newFake(t, &fakeHub{files: map[string][]byte{"sub/model.gguf": data}})
	dest := filepath.Join(t.TempDir(), "models", "model.gguf")
	var last int64
	err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "sub/model.gguf", Size: int64(len(data)), SHA256: sum}, dest,
		func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded content differs")
	}
	if last != int64(len(data)) {
		t.Errorf("progress ended at %d, want %d", last, len(data))
	}
	if _, err := os.Stat(dest + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Error(".part left behind")
	}
}

func TestDownloadResumesWithRange(t *testing.T) {
	data, sum := testData(2<<20 + 5)
	f := &fakeHub{files: map[string][]byte{"m.bin": data}}
	c, _ := newFake(t, f)
	dest := filepath.Join(t.TempDir(), "m.bin")
	if err := os.WriteFile(dest+".part", data[:1<<20], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "m.bin", Size: int64(len(data)), SHA256: sum}, dest, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatal("resumed content differs")
	}
	if len(f.ranges) != 1 || f.ranges[0] != fmt.Sprintf("bytes=%d-", 1<<20) {
		t.Errorf("Range headers sent: %q", f.ranges)
	}
}

func TestDownloadRestartsWhenServerIgnoresRange(t *testing.T) {
	data, sum := testData(1<<20 + 99)
	c, _ := newFake(t, &fakeHub{files: map[string][]byte{"m.bin": data}, ignoreRange: true})
	dest := filepath.Join(t.TempDir(), "m.bin")
	// A .part of the wrong bytes: only a full restart can still end right.
	if err := os.WriteFile(dest+".part", bytes.Repeat([]byte{'x'}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "m.bin", Size: int64(len(data)), SHA256: sum}, dest, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatal("content differs after a restart")
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	data, _ := testData(70000)
	_, other := testData(70001)
	c, _ := newFake(t, &fakeHub{files: map[string][]byte{"m.bin": data}})
	dest := filepath.Join(t.TempDir(), "m.bin")
	err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "m.bin", Size: int64(len(data)), SHA256: other}, dest, nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	for _, p := range []string{dest, dest + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left after a bad checksum", filepath.Base(p))
		}
	}
}

func TestDownloadSkipsCompleteFile(t *testing.T) {
	data, sum := testData(5000)
	f := &fakeHub{files: map[string][]byte{"m.bin": data}}
	c, _ := newFake(t, f)
	dest := filepath.Join(t.TempDir(), "m.bin")
	os.WriteFile(dest, data, 0o644)
	if err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "m.bin", Size: int64(len(data)), SHA256: sum}, dest, nil); err != nil {
		t.Fatal(err)
	}
	if f.resolveHits != 0 {
		t.Errorf("a complete file was fetched again (%d requests)", f.resolveHits)
	}
}

func TestDownloadReplacesWrongExistingFile(t *testing.T) {
	data, sum := testData(5000)
	c, _ := newFake(t, &fakeHub{files: map[string][]byte{"m.bin": data}})
	dest := filepath.Join(t.TempDir(), "m.bin")
	os.WriteFile(dest, bytes.Repeat([]byte{'z'}, 5000), 0o644)
	if err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "m.bin", Size: 5000, SHA256: sum}, dest, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatal("a wrong file in place was not replaced")
	}
}

func TestDownloadResumesAfterStall(t *testing.T) {
	oldIdle, oldWait := idleTimeout, retryWait
	idleTimeout, retryWait = 300*time.Millisecond, func(int) time.Duration { return 0 }
	defer func() { idleTimeout, retryWait = oldIdle, oldWait }()

	data, sum := testData(1<<20 + 1234)
	f := &fakeHub{files: map[string][]byte{"m.bin": data}, stallFirst: true}
	c, _ := newFake(t, f)
	dest := filepath.Join(t.TempDir(), "m.bin")
	if err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "m.bin", Size: int64(len(data)), SHA256: sum}, dest, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatal("content differs after a stall")
	}
	if f.resolveHits != 2 || f.ranges[1] != fmt.Sprintf("bytes=%d-", len(data)/2) {
		t.Errorf("after a stall: %d requests, ranges %q; want 2 and a resume from the half", f.resolveHits, f.ranges)
	}
}

func TestDownloadNotFound(t *testing.T) {
	c, _ := newFake(t, &fakeHub{files: map[string][]byte{}})
	err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "nope.bin", Size: 3}, filepath.Join(t.TempDir(), "x"), nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRedirectToAnotherHostDropsAuthorization(t *testing.T) {
	data, sum := testData(4000)
	var cdnAuth []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = append(cdnAuth, r.Header.Get("Authorization"))
		http.ServeContent(w, r, "blob", time.Unix(1, 0), bytes.NewReader(data))
	}))
	defer cdn.Close()
	var hubAuth string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, cdn.URL+"/blob?X-Amz-Signature=sig", http.StatusFound)
	}))
	defer hub.Close()

	c := &Client{Endpoint: hub.URL, Token: "secret-token"}
	dest := filepath.Join(t.TempDir(), "m.bin")
	if err := c.Download(context.Background(), "org/repo", testCommit, File{Path: "m.bin", Size: int64(len(data)), SHA256: sum}, dest, nil); err != nil {
		t.Fatal(err)
	}
	if hubAuth != "Bearer secret-token" {
		t.Errorf("the Hub got Authorization %q", hubAuth)
	}
	if len(cdnAuth) != 1 || cdnAuth[0] != "" {
		t.Errorf("the token followed the redirect to another host: %q", cdnAuth)
	}
}

func TestStatusErrorHidesSignedQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := retryWait
	retryWait = func(int) time.Duration { return 0 }
	defer func() { retryWait = old }()
	err := DownloadURL(context.Background(), srv.Client(), srv.URL+"/f?X-Amz-Signature=topsecret", filepath.Join(t.TempDir(), "f"), 0, "", nil, nil)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("err = %v (must fail and must not show the signature)", err)
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("a 500 was not retried: %v", err)
	}
}

func TestDownloadURLUnknownSize(t *testing.T) {
	data, _ := testData(10000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "yes" {
			http.Error(w, "missing header", http.StatusBadRequest)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f")
	if err := DownloadURL(context.Background(), nil, srv.URL+"/f", dest, 0, "", map[string]string{"X-Test": "yes"}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if err := DownloadURL(context.Background(), nil, srv.URL+"/f", dest, 0, "nothex", nil, nil); err == nil {
		t.Error("a malformed sha256 was accepted")
	}
}

func TestSearchQueryAndGated(t *testing.T) {
	var q map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		q = r.URL.Query()
		fmt.Fprint(w, `[{"id":"a/b","downloads":10,"likes":2,"pipeline_tag":"automatic-speech-recognition","library_name":"nemo",
			"tags":["license:cc-by-4.0"],"gated":"manual","lastModified":"2025-08-14T10:00:00.000Z"},
			{"modelId":"c/d","downloads":5,"gated":false}]`)
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, HTTP: srv.Client()}
	got, err := c.Search(context.Background(), SearchQuery{Search: "parakeet", Author: "nvidia", Filter: []string{"onnx", "license:apache-2.0"},
		PipelineTag: "automatic-speech-recognition", Sort: "trending_score", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{"search": "parakeet", "author": "nvidia", "pipeline_tag": "automatic-speech-recognition",
		"sort": "trendingScore", "direction": "-1", "limit": "5"}
	for k, v := range checks {
		if len(q[k]) != 1 || q[k][0] != v {
			t.Errorf("query %s = %q, want %q", k, q[k], v)
		}
	}
	if strings.Join(q["filter"], ",") != "onnx,license:apache-2.0" {
		t.Errorf("filters = %q", q["filter"])
	}
	if !strings.Contains(strings.Join(q["expand"], ","), "gated") {
		t.Errorf("expand = %q", q["expand"])
	}
	if len(got) != 2 || got[0].ID != "a/b" || !got[0].Gated || got[0].Library != "nemo" || got[0].LastModified.Year() != 2025 {
		t.Errorf("first result %+v", got[0])
	}
	if got[1].ID != "c/d" || got[1].Gated {
		t.Errorf("second result %+v", got[1])
	}
	if _, err := c.Search(context.Background(), SearchQuery{Sort: "random"}); err == nil {
		t.Error("an unknown sort was accepted")
	}
}

func TestGitHubReleaseAssets(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/tags/b1":
			if r.Header.Get("User-Agent") == "" {
				t.Error("no User-Agent sent to GitHub")
			}
			fmt.Fprintf(w, `{"tag_name":"b1","assets":[
				{"name":"llama-b1-bin-win-cpu-x64.zip","size":123,"browser_download_url":"https://example/x64.zip","digest":"sha256:%s"},
				{"name":"llama-b1-bin-win-cpu-arm64.zip","size":456,"browser_download_url":"https://example/arm64.zip","digest":null},
				{"name":"cudart-llama-bin-win-cuda-12.4-x64.zip","size":7,"browser_download_url":"https://example/cudart.zip"}]}`, strings.ToUpper(digest))
		case "/repos/o/limited/releases/tags/b1":
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = old }()
	ctx := context.Background()

	a, err := ReleaseAsset(ctx, srv.Client(), "o/r", "b1", "llama-b1-bin-win-cpu-x64.zip")
	if err != nil || a.Size != 123 || a.SHA256 != digest || a.URL != "https://example/x64.zip" {
		t.Fatalf("ReleaseAsset = %+v, %v", a, err)
	}
	a, err = ReleaseAssetMatching(ctx, srv.Client(), "o/r", "b1", func(n string) bool { return strings.HasSuffix(n, "win-cpu-arm64.zip") })
	if err != nil || a.SHA256 != "" || a.Size != 456 {
		t.Errorf("asset without digest = %+v, %v", a, err)
	}
	if _, err := ReleaseAssetMatching(ctx, srv.Client(), "o/r", "b1", func(n string) bool { return strings.Contains(n, "win-cpu") }); err == nil || !strings.Contains(err.Error(), "2 assets match") {
		t.Errorf("ambiguous match: %v", err)
	}
	if _, err := ReleaseAssetMatching(ctx, srv.Client(), "o/r", "b1", func(n string) bool { return strings.Contains(n, "macos") }); !errors.Is(err, ErrNotFound) {
		t.Errorf("no match: %v, want ErrNotFound", err)
	}
	if _, err := ReleaseAsset(ctx, srv.Client(), "o/r", "b9", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag: %v, want ErrNotFound", err)
	}
	_, err = ReleaseAsset(ctx, srv.Client(), "o/limited", "b1", "x")
	if err == nil || errors.Is(err, ErrGated) || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("rate limit: %v", err)
	}
	if _, err := ReleaseAsset(ctx, srv.Client(), "not-a-repo", "b1", "x"); err == nil {
		t.Error("a bad repo was accepted")
	}
}

func TestLocalPath(t *testing.T) {
	dir := t.TempDir()
	ok := map[string]string{"a/b.gguf": filepath.Join(dir, "a", "b.gguf"), "./x/./y": filepath.Join(dir, "x", "y"), `w\v.txt`: filepath.Join(dir, "w", "v.txt")}
	for in, want := range ok {
		if got, err := LocalPath(dir, in); err != nil || got != want {
			t.Errorf("LocalPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "..", "../x", "a/../../x", "/etc/passwd", "C:/x", `C:\x`, "."} {
		if got, err := LocalPath(dir, bad); err == nil {
			t.Errorf("LocalPath(%q) = %q, want an error", bad, got)
		}
	}
}

func TestProgressPrinterThrottlesAndFinishes(t *testing.T) {
	var buf bytes.Buffer
	p := ProgressPrinter(&buf, "model.gguf")
	for i := int64(0); i < 1000; i++ {
		p(i*1000, 1000000)
	}
	p(1000000, 1000000)
	p(1000000, 1000000) // after the end: ignored
	out := buf.String()
	if n := strings.Count(out, "\r"); n > 3 {
		t.Errorf("%d updates for a fast download; throttling failed: %q", n, out)
	}
	if !strings.HasSuffix(out, "100%\n") || strings.Count(out, "\n") != 1 {
		t.Errorf("output %q does not end once at 100%%", out)
	}
	var unknown bytes.Buffer
	ProgressPrinter(&unknown, "f")(2500000, -1)
	if !strings.Contains(unknown.String(), "2.5 MB") {
		t.Errorf("unknown total printed %q", unknown.String())
	}
}

func TestContentRange(t *testing.T) {
	cases := map[string][3]int64{"bytes 100-199/200": {100, 200, 1}, "bytes 0-9/*": {0, -1, 1}, "items 1-2/3": {0, 0, 0}, "bytes x-2/3": {0, 0, 0}}
	for in, want := range cases {
		s, tot, ok := contentRange(in)
		if (ok && want[2] == 0) || (!ok && want[2] == 1) || (ok && (s != want[0] || tot != want[1])) {
			t.Errorf("contentRange(%q) = %d, %d, %v", in, s, tot, ok)
		}
	}
}
