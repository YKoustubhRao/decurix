package skills

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"thebot/internal/netguard"
	"thebot/internal/workspace"
)

func newDownloadRC(t *testing.T) RunContext {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New() error = %v", err)
	}
	// These tests' pages are httptest servers on loopback, which the guarded
	// client refuses (TestDownloadRefusesNonPublicAddresses).
	old := fetchClient
	fetchClient = http.DefaultClient
	t.Cleanup(func() { fetchClient = old })
	return RunContext{Ctx: context.Background(), Workspace: ws, OnLog: func(string) {}}
}

// "download the PDF files from the URL found" (the phrasing a 2-step
// search-then-download plan produces) names no URL. The URL an earlier step's
// prose mentions is NOT taken: that prose may be a model's summary of a page
// (or a page's injected text), not a page anything fetched. Only what a
// verified step recorded fetching (Meta, TestDownloadRecoversURLWhenSearchSynthesisWasWrong) is.
func TestDownloadDoesNotTakeAURLFromPriorProse(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("%PDF-1.4 report"))
	}))
	defer srv.Close()
	fileURL := srv.URL + "/report.pdf"

	rc := newDownloadRC(t)
	rc.PriorResults = []Result{{
		Subtask:  Subtask{Step: "search the web for the filing"},
		Output:   "Found the filing at " + fileURL + " according to the source.",
		Verified: true,
		Meta:     map[string]any{MetaModelReply: true},
	}}

	res, err := NewDownload().Run(rc, Subtask{Step: "download the PDF files from the URL found", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Verified || hits != 0 {
		t.Fatalf("downloaded a URL only a model's summary named (verified=%v, hits=%d): %s", res.Verified, hits, res.Output)
	}
}

// A plan step naming a URL neither the request nor a fetched page gave is not
// followed - a planner (a model) may have copied it from a summary.
func TestDownloadIgnoresAStepURLTheRequestDidNotGive(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("%PDF-1.4 x"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	rc.OriginalPrompt = "find the annual report and download it"
	res, _ := NewDownload().Run(rc, Subtask{Step: "download " + srv.URL + "/annual.pdf", Type: "download"})
	if res.Verified || hits != 0 {
		t.Fatalf("followed a URL the request never gave (verified=%v, hits=%d): %s", res.Verified, hits, res.Output)
	}

	rc.OriginalPrompt = "download " + srv.URL + "/annual.pdf please"
	res, _ = NewDownload().Run(rc, Subtask{Step: "download " + srv.URL + "/annual.pdf", Type: "download"})
	if !res.Verified {
		t.Fatalf("a URL the request gave was not downloaded: %s", res.Output)
	}
}

// The guarded client never reaches loopback (the control plane's published
// port, soul, the agent API, Chrome's debugging port), private or link-local
// addresses (the metadata service) - including through a redirect.
func TestDownloadRefusesNonPublicAddresses(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("%PDF-1.4 internal"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	fetchClient = netguard.Client(0) // the production client, not the test's
	for _, u := range []string{
		srv.URL + "/x.pdf",
		"http://169.254.169.254/latest/meta-data/iam.json",
		"http://10.0.0.1:8080/api/desktops.json",
		"http://[::1]:4900/report.pdf",
		"http://localhost:9222/json",
	} {
		res, _ := NewDownload().Run(rc, Subtask{Step: "download " + u, Type: "download"})
		if res.Verified {
			t.Errorf("downloaded %s: %s", u, res.Output)
		}
	}
	if hits != 0 {
		t.Errorf("the loopback server was reached %d time(s)", hits)
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
// output that reflects both places were checked.
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
		if !slices.Contains(exts, "cpp") || !slices.Contains(exts, "hpp") {
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

// A login page served under invoice.pdf is not an invoice: it is not saved
// and the step is not verified.
func TestDownloadRefusesAPageServedUnderAFileName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!DOCTYPE html><html><body><form>Sign in</form></body></html>"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	res, _ := NewDownload().Run(rc, Subtask{Step: "download " + srv.URL + "/invoice.pdf", Type: "download"})
	if res.Verified {
		t.Fatalf("an HTML page saved as invoice.pdf was verified: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(rc.Workspace.Root(), "downloads", "invoice.pdf")); err == nil {
		t.Error("the HTML page was saved as invoice.pdf")
	}
	if !strings.Contains(res.Output, "not a PDF") {
		t.Errorf("Output = %q, want it to say what came back", res.Output)
	}
}

// Every file asked for must be saved: 1 of 2 is not the download asked for.
func TestDownloadVerifiesOnlyWhenEveryFileIsSaved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list":
			w.Write([]byte(`<a href="/a.pdf">a</a><a href="/b.pdf">b</a>`))
		case "/a.pdf":
			w.Write([]byte("%PDF-1.4 a"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	res, _ := NewDownload().Run(rc, Subtask{Step: "download the pdf files from " + srv.URL + "/list", Type: "download"})
	if res.Verified {
		t.Fatalf("1 of 2 files was verified: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1 of 2") {
		t.Errorf("Output = %q, want it to say how much was saved", res.Output)
	}
}

// One fetch never reads without bound.
func TestFetchBytesIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()
	newDownloadRC(t) // lets this test reach its loopback server
	if _, err := fetchBytes(context.Background(), srv.URL, 5*time.Second, 1024); err == nil {
		t.Fatal("a 4 KB body was read whole under a 1 KB bound")
	}
	if data, err := fetchBytes(context.Background(), srv.URL, 5*time.Second, 8192); err != nil || len(data) != 4096 {
		t.Fatalf("within the bound: %d bytes, %v", len(data), err)
	}
}

func TestContentMismatch(t *testing.T) {
	for _, c := range []struct {
		ext, data string
		ok        bool
	}{
		{"pdf", "%PDF-1.7\n...", true},
		{"pdf", "<html>login</html>", false},
		{"xlsx", "PK\x03\x04rest", true},
		{"xlsx", "<!doctype html>", false},
		{"csv", "a,b\n1,2\n", true},
		{"csv", "<html><body>error</body></html>", false},
		{"json", `{"a": 1}`, true},
		{"json", "<html></html>", false},
		{"html", "<html></html>", true},
		{"html", "\x1f\x8b\x08\x00gz", false},
	} {
		if got := contentMismatch(c.ext, []byte(c.data)) == ""; got != c.ok {
			t.Errorf("contentMismatch(%s, %q) ok = %v, want %v", c.ext, c.data, got, c.ok)
		}
	}
}

// A later job's download never overwrites an earlier job's file of the same name (review T-30).
func TestADownloadKeepsAnEarlierJobsFile(t *testing.T) {
	body := "a,b\n1,first\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	rc := newDownloadRC(t)
	if res, _ := NewDownload().Run(rc, Subtask{Step: "download " + srv.URL + "/report.csv", Type: "download"}); !res.Verified {
		t.Fatalf("first download: %+v", res)
	}
	body = "a,b\n2,second\n"
	res, _ := NewDownload().Run(rc, Subtask{Step: "download " + srv.URL + "/report.csv", Type: "download"})
	if !res.Verified || !strings.Contains(res.Output, "already there") {
		t.Fatalf("second download: %+v", res)
	}
	first, err := os.ReadFile(filepath.Join(rc.Workspace.Root(), "downloads", "report.csv"))
	if err != nil || string(first) != "a,b\n1,first\n" {
		t.Fatalf("the earlier file was overwritten: %q %v", first, err)
	}
	if second, err := os.ReadFile(filepath.Join(rc.Workspace.Root(), "downloads", "report_2.csv")); err != nil || string(second) != "a,b\n2,second\n" {
		t.Fatalf("the new file: %q %v", second, err)
	}
}
