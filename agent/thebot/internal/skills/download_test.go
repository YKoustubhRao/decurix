package skills

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/workspace"
)

func newDownloadRC(t *testing.T) RunContext {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New() error = %v", err)
	}
	return RunContext{Ctx: context.Background(), Workspace: ws, OnLog: func(string) {}}
}

// The real bug this pins: "download the PDF files from the URL found"
// (the exact phrasing a 2-step search-then-download plan produces) has
// no URL in its own text. Without falling back to what the earlier
// search step actually found, this failed on every single compound
// download request.
func TestDownloadFallsBackToURLFromPriorFindings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`report.pdf`))
	}))
	defer srv.Close()
	fileURL := srv.URL + "/report.pdf"

	rc := newDownloadRC(t)
	rc.PriorResults = []Result{{
		Subtask:  Subtask{Step: "search the web for the filing"},
		Output:   "Found the filing at " + fileURL + " according to the source.",
		Verified: true,
	}}

	res, err := NewDownload().Run(rc, Subtask{Step: "download the PDF files from the URL found", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}
	if !strings.Contains(res.Output, "report.pdf") {
		t.Errorf("Output = %q, want it to reflect the downloaded file", res.Output)
	}
}

// An explicit URL in the step itself must still win - the fallback only
// applies when the step names nothing at all.
func TestDownloadPrefersURLInTheStepOverPriorFindings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	rc.PriorResults = []Result{{
		Subtask: Subtask{Step: "search"}, Output: "a decoy at http://example.invalid/wrong.pdf", Verified: true,
	}}

	res, _ := NewDownload().Run(rc, Subtask{Step: "download " + srv.URL + "/real.pdf", Type: "download"})
	if strings.Contains(res.Output, "wrong.pdf") {
		t.Errorf("Output = %q, used the decoy URL instead of the one named in the step", res.Output)
	}
}

// No URL anywhere - own step or prior findings - must fail honestly with
// a message that reflects both places were checked.
func TestDownloadNoURLAnywhereFailsHonestly(t *testing.T) {
	rc := newDownloadRC(t)
	res, err := NewDownload().Run(rc, Subtask{Step: "download the files", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Verified {
		t.Error("Verified = true with no URL anywhere, want false")
	}
}

// The live bug this pins: "download the C++ source files from the search
// results" (a real GitHub repo it had just found) fell back to the
// default doc-type extension list, matched an incidental manifest.json on
// the repo's landing page, and never touched the actual source it was
// sent to fetch.
func TestWantedExtsRecognisesSourceCode(t *testing.T) {
	for _, step := range []string{
		"download the C++ source files from the search results",
		"download the cpp source code",
		"download the source files",
	} {
		exts := wantedExts(step)
		if !contains(exts, "cpp") || !contains(exts, "hpp") {
			t.Errorf("wantedExts(%q) = %v, want cpp/hpp included", step, exts)
		}
	}
}

func TestDownloadFindsCppSourceFilesOnAPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repo":
			w.Write([]byte(`<a href="/repo/manifest.json">manifest</a><a href="/repo/sssp.hpp">sssp.hpp</a>`))
		default:
			w.Write([]byte("// real header-only source\n"))
		}
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	res, err := NewDownload().Run(rc, Subtask{Step: "download the C++ source files from " + srv.URL + "/repo", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}
	if strings.Contains(res.Output, "manifest.json") {
		t.Errorf("Output = %q, downloaded the manifest instead of/as well as the real source", res.Output)
	}
	if !strings.Contains(res.Output, "sssp.hpp") {
		t.Errorf("Output = %q, want the real .hpp source file downloaded", res.Output)
	}
}

// The live bug this pins: a server that gzip-compresses its response (the
// HTML-bulk-download case revealed this - ordinary text servers routinely
// do) produced a saved file starting with the raw gzip magic header
// \x1f\x8b, because the old code manually set Accept-Encoding, which
// tells Go's transport "I'll decompress this myself" and it silently
// stops doing so. docling then correctly refused to parse binary garbage
// with a .html extension - the real bug was upstream of that refusal.
func TestFetchBytesDecompressesGzipResponses(t *testing.T) {
	const real = "<html><body>real decompressed content, not gzip bytes</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte(real))
		gz.Close()
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	res, err := NewDownload().Run(rc, Subtask{Step: "download the raw html of " + srv.URL + "/page", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}

	entries, err := os.ReadDir(filepath.Join(rc.Workspace.Root(), "downloads"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one saved file, got %v (err=%v)", entries, err)
	}
	saved, err := os.ReadFile(filepath.Join(rc.Workspace.Root(), "downloads", entries[0].Name()))
	if err != nil {
		t.Fatalf("could not read saved file: %v", err)
	}
	if bytes.HasPrefix(saved, []byte{0x1f, 0x8b}) {
		t.Fatal("saved file starts with the raw gzip magic header - it was never decompressed")
	}
	if string(saved) != real {
		t.Errorf("saved content = %q, want the real decompressed html %q", saved, real)
	}
}

// The live case this pins: "download the raw html of all 10 pages" after
// a search step, where the pages are ordinary extension-less URLs
// ("/director/abhishek-gadia/08012252") - not files linked FROM one page.
// Every one of the prior step's real URLs must be saved as itself.
func TestDownloadBulkSavesRawHTMLOfMultiplePages(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.Write([]byte("<html><body>real page for " + r.URL.Path + "</body></html>"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	rc.PriorResults = []Result{{
		Subtask:  Subtask{Step: "search the web for Abhishek Gadia"},
		Output:   "Found several director records.",
		Verified: true,
		Meta: map[string]any{"sources": []string{
			srv.URL + "/director/abhishek-gadia/08012252",
			srv.URL + "/people-profile/abhishek-gadia/08012252",
			srv.URL + "/company/qube-research",
		}},
	}}

	res, err := NewDownload().Run(rc, Subtask{
		Step: "download the raw html of all 10 pages using a browser", Type: "download",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}
	if len(hits) != 3 {
		t.Fatalf("server received %d request(s), want 3 - every prior URL should be fetched directly, none scanned for links", len(hits))
	}

	saved, _ := res.Meta["paths"].([]string)
	if len(saved) != 3 {
		t.Fatalf("Meta[paths] = %v, want 3 saved files", saved)
	}
	entries, err := os.ReadDir(filepath.Join(rc.Workspace.Root(), "downloads"))
	if err != nil {
		t.Fatalf("could not read downloads dir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("downloads/ has %d file(s), want 3 (extension-less URLs must not collide and overwrite each other)", len(entries))
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".html") {
			t.Errorf("saved file %q has no .html extension even though the source URL had none", e.Name())
		}
	}
}

// A single named page asking for raw html must save the page itself, not
// try to scan its own hrefs for other html files (which would almost
// never find anything real).
func TestDownloadSinglePageRawHTMLSavesThePageItself(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body>the real page, with an unrelated <a href=\"/other\">link</a></body></html>"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	res, err := NewDownload().Run(rc, Subtask{Step: "download the raw html of " + srv.URL + "/profile", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}
	saved, _ := res.Meta["paths"].([]string)
	if len(saved) != 1 {
		t.Fatalf("Meta[paths] = %v, want exactly 1 (the page itself, not its links)", saved)
	}
}

// The direct regression for the UA block: a server that 403s a browser
// UA but serves a declared one must still succeed, exactly like sec.gov
// does live.
func TestDownloadRecoversFromUABlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == browserUA {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("%PDF-1.4 real bytes"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	res, err := NewDownload().Run(rc, Subtask{Step: "download " + srv.URL + "/filing.pdf", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified after a UA-block recovery: %s", res.Output)
	}

	saved, _ := res.Meta["paths"].([]string)
	if len(saved) != 1 {
		t.Fatalf("Meta[paths] = %v, want exactly one saved file", saved)
	}
	abs := filepath.Join(rc.Workspace.Root(), "downloads", "filing.pdf")
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("could not read the saved file: %v", err)
	}
	if string(data) != "%PDF-1.4 real bytes" {
		t.Errorf("saved file content = %q, want the real fetched bytes", data)
	}
}
