package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"thebot/internal/netguard"
)

// DownloadSkill finds REAL downloadable links in a page's actual HTML and
// saves the REAL bytes. Never invents a URL - the reason it exists, from
// the Node prototype: without it, "download the excel files from <page>"
// fell through to text generation and the model hallucinated a direct
// file URL that 404'd. Every link downloaded here was actually found in
// the real page.
type DownloadSkill struct{}

func NewDownload() *DownloadSkill { return &DownloadSkill{} }

func (d *DownloadSkill) Name() string { return "download" }
func (d *DownloadSkill) Description() string {
	return "Downloads real files linked from a real page (or a direct file URL) into the workspace - never a guessed link."
}

func (d *DownloadSkill) Matches(subtask Subtask) bool { return subtask.Type == "download" }

const (
	downloadDir  = "downloads"
	maxDownloads = 20
)

var (
	hrefRe     = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
	extRe      = regexp.MustCompile(`(?i)\.([a-z0-9]{2,5})(?:[?#]|$)`)
	unsafeName = regexp.MustCompile(`[^\w.\-]+`)

	allExts = []string{"xlsx", "xls", "csv", "pdf", "docx", "doc", "pptx", "ppt", "zip", "json"}
	extWord = map[string][]string{
		"excel": {"xlsx", "xls"}, "spreadsheet": {"xlsx", "xls"}, "csv": {"csv"},
		"pdf": {"pdf"}, "word": {"docx", "doc"}, "powerpoint": {"pptx", "ppt"},
		// Deliberately not in allExts's unqualified-fallback default -
		// only requested explicitly. "the raw html" / "the pages" must
		// name what they want; a bare "download the files" shouldn't
		// start scooping up every incidental .html link on a page.
		"html": {"html", "htm"}, "raw html": {"html", "htm"}, "web pages": {"html", "htm"},
		"webpages": {"html", "htm"}, "web page": {"html", "htm"}, "webpage": {"html", "htm"},
		// Same reasoning for source code. Live gap: "download the C++
		// source files from the search results" (a real GitHub repo
		// implementing a real algorithm it had just found) fell back to
		// the default doc-type list, matched only an incidental
		// manifest.json on the repo's landing page, and never touched the
		// actual .cpp/.hpp source it was sent to fetch.
		"c++": {"cpp", "hpp", "h", "cc", "cxx"}, "cpp": {"cpp", "hpp", "h", "cc", "cxx"},
		"source code":  {"cpp", "hpp", "h", "cc", "cxx", "py", "go", "rs", "java", "js", "ts"},
		"source files": {"cpp", "hpp", "h", "cc", "cxx", "py", "go", "rs", "java", "js", "ts"},
	}
)

// wantedExts maps what the request actually named - "the excel files"
// means xlsx/xls, not every PDF on the page too. Nothing named means
// every recognised type, which is genuinely "all", not a guess.
func wantedExts(step string) []string {
	lower := strings.ToLower(step)
	seen := map[string]bool{}
	var out []string
	add := func(e string) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for word, exts := range extWord {
		if strings.Contains(lower, word) {
			for _, e := range exts {
				add(e)
			}
		}
	}
	for _, e := range allExts {
		if regexp.MustCompile(`\b` + e + `\b`).MatchString(lower) {
			add(e)
		}
	}
	if len(out) == 0 {
		return allExts
	}
	return out
}

// wantsHTML reports whether the resolved extension set includes raw HTML
// pages - the signal that switches the skill from "scan this page for
// linked files" to "save these pages themselves".
func wantsHTML(exts []string) bool {
	return slices.Contains(exts, "html") || slices.Contains(exts, "htm")
}

// extractLinks pulls every href matching a wanted extension out of the
// REAL fetched page, resolved against the page it was found on -
// site-relative paths like "/files/report.xls" resolve wrong against any
// other base.
func extractLinks(html, pageURL string, exts []string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
		raw := strings.ReplaceAll(m[1], "&amp;", "&")
		em := extRe.FindStringSubmatch(raw)
		if em == nil || !slices.Contains(exts, strings.ToLower(em[1])) {
			continue
		}
		ref, err := url.Parse(raw)
		if err != nil {
			continue
		}
		abs := base.ResolveReference(ref).String()
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	return out
}

// browserUA/declaredUA mirror internal/websearch's split: most sites want
// to see a browser, some (sec.gov confirmed live) hard-403 a browser UA
// and only serve a client that declares itself honestly, per their own
// published access policy - so a 403 gets exactly one retry as that
// declared identity before being reported as a real failure.
const (
	browserUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	declaredUA = "TheBot-Research/1.0 (automated research agent; contact: research@thebot.local)"
)

// maxPageBytes and maxFileBytes bound what one fetch may hold in memory: a
// page scanned for links, and one downloaded file. Past the bound the fetch
// fails rather than reading on without end.
const (
	maxPageBytes = 20 << 20
	maxFileBytes = 200 << 20
)

// fetchClient dials only public internet addresses and re-checks every
// redirect (netguard): a link a model or a page named never reaches the
// control plane, this desktop's own services or a metadata endpoint. Tests swap it.
var fetchClient = netguard.Client(0)

// fetchBytesAs fetches target as ua, reading at most limit bytes.
func fetchBytesAs(ctx context.Context, target, ua string, timeout time.Duration, limit int64) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", ua)
	// Deliberately NOT setting Accept-Encoding. Go's http.Transport has a
	// real, documented behavior here: when the caller leaves it unset, the
	// transport requests gzip itself and transparently decompresses the
	// response. Setting the header explicitly - which this used to do -
	// tells the transport "I'll handle decompression myself" and it stops
	// doing so, silently. Found live: a downloaded "raw html" file started
	// with bytes \x1f\x8b\x08\x00 - the gzip magic header - saved to disk
	// as-is; docling then correctly refused to parse a binary file with a
	// .html extension. Every download this skill had ever done was at
	// real risk of the same silent corruption whenever the server
	// happened to compress its response; PDFs/binaries just rarely are.
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(body)) > limit {
		return nil, resp.StatusCode, fmt.Errorf("larger than %d MB - not downloaded", limit>>20)
	}
	return body, resp.StatusCode, err
}

// fetchBytes fetches target, at most limit bytes, retrying a 403 once as the declared client.
func fetchBytes(ctx context.Context, target string, timeout time.Duration, limit int64) ([]byte, error) {
	data, status, err := fetchBytesAs(ctx, target, browserUA, timeout, limit)
	if err != nil && status == http.StatusForbidden {
		data, _, err = fetchBytesAs(ctx, target, declaredUA, timeout, limit)
	}
	return data, err
}

func (d *DownloadSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	if rc.Workspace == nil {
		return Result{Subtask: subtask, SkillName: d.Name(), Output: "No workspace configured.", Verified: false, Meta: map[string]any{}}, nil
	}

	exts := wantedExts(subtask.Step)
	target := givenURL(rc, subtask.Step)

	var fileURLs []string
	// Bulk mode: "download the raw html of these N pages" (typically the
	// results of a preceding search step) asks for each URL to be saved
	// AS ITSELF, not scanned for further links. This is a genuinely
	// different shape from "download the files linked from this one
	// page" - conflating them would mean fetching only the first result
	// and scraping ITS html for further html-extension hrefs, which is
	// not what was asked and would almost never find anything (ordinary
	// pages don't cross-link a pile of other raw .html files).
	if target == "" && wantsHTML(exts) {
		if candidates := rc.PriorSourceURLs(); len(candidates) > 1 {
			fileURLs = candidates
			rc.Logf("download", "no single URL in this step - saving all %d page(s) an earlier step found, as raw html", len(fileURLs))
		}
	}

	if len(fileURLs) == 0 {
		if target == "" {
			// The step itself named no URL - the live failure this fixes
			// was literally "download the PDF files from the URL found",
			// where "found" refers to the web-search step just before it.
			// Without this, a perfectly good 2-step plan (search, then
			// download what it found) died on step 2 every time.
			//
			// PriorSourceURLs() is the REAL fetched/found URLs,
			// structurally recorded - not a regex scrape of the previous
			// step's prose. That distinction mattered live: a search step
			// fetched the real SEC filing but its own synthesis misjudged
			// the page and wrote "no information found", citing no URL in
			// its answer text at all. The real link was still sitting in
			// that step's Meta the whole time.
			// Never a URL an earlier step's prose mentions: that text may be
			// a model's summary (or a page's injected words), not a page
			// anything fetched.
			candidates := rc.PriorSourceURLs()
			if len(candidates) > 0 {
				// Prefer a candidate that's already a direct file of the
				// wanted type over one that's just a page to go scan.
				target = candidates[0]
				for _, c := range candidates {
					if em := extRe.FindStringSubmatch(c); em != nil && slices.Contains(exts, strings.ToLower(em[1])) {
						target = c
						break
					}
				}
				rc.Logf("download", "no URL in this step - using one an earlier step actually fetched: %s", target)
			}
		}
		if target == "" {
			return Result{
				Subtask: subtask, SkillName: d.Name(),
				Output: "No URL in the request or in what earlier steps found - name the page or file to download from.", Verified: false, Meta: map[string]any{},
			}, nil
		}
		rc.Logf("download", "looking for real %s links at %s", strings.Join(exts, "/"), target)

		if em := extRe.FindStringSubmatch(target); em != nil && slices.Contains(exts, strings.ToLower(em[1])) {
			fileURLs = []string{target} // the URL IS the file
		} else if wantsHTML(exts) {
			// A single page was named and raw html was asked for - the
			// page itself is the wanted file, not something to scan for
			// further links (its own hrefs are page navigation, not a
			// list of other pages to download).
			fileURLs = []string{target}
		} else {
			body, err := fetchBytes(rc.Ctx, target, 25*time.Second, maxPageBytes)
			if err != nil {
				return Result{
					Subtask: subtask, SkillName: d.Name(),
					Output: "Could not load " + target + ": " + err.Error(), Verified: false,
					Meta: map[string]any{"error": err.Error()},
				}, nil
			}
			fileURLs = extractLinks(string(body), target, exts)
			rc.Logf("download", "found %d real matching link(s) on the page", len(fileURLs))
			if len(fileURLs) == 0 {
				return Result{
					Subtask: subtask, SkillName: d.Name(),
					Output: fmt.Sprintf("No real %s links in %s's HTML. The page may use other file types, or add its links via JavaScript (which a plain fetch can't see).",
						strings.Join(exts, "/"), target),
					Verified: false, Meta: map[string]any{"checked": target},
				}, nil
			}
		}
	}

	leftOut := 0
	if len(fileURLs) > maxDownloads {
		rc.Logf("download", "capping at %d of %d found links", maxDownloads, len(fileURLs))
		leftOut = len(fileURLs) - maxDownloads
		fileURLs = fileURLs[:maxDownloads]
	}

	var saved []string
	var failed []string
	usedNames := map[string]bool{}
	for i, fileURL := range fileURLs {
		rc.Logf("download", "downloading %d/%d: %s", i+1, len(fileURLs), fileURL)
		data, err := fetchBytes(rc.Ctx, fileURL, 60*time.Second, maxFileBytes)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %s", fileURL, err.Error()))
			continue
		}
		// What came back must be the kind of file asked for: a login page
		// served under invoice.pdf is not an invoice.
		if ext := fileExt(fileURL); ext != "" {
			if why := contentMismatch(ext, data); why != "" {
				failed = append(failed, fmt.Sprintf("%s: not saved - %s", fileURL, why))
				continue
			}
		}

		parsed, _ := url.Parse(fileURL)
		name, _ := url.QueryUnescape(path.Base(parsed.Path))
		name = unsafeName.ReplaceAllString(name, "_")
		if len(name) > 150 {
			name = name[:150]
		}
		if name == "" || name == "_" {
			name = fmt.Sprintf("download-%d", i+1)
		}
		// Bulk raw-html saves come from real pages that routinely have
		// extension-less URLs (clean-URL routing, not a static .html file)
		// - "/director/abhishek-gadia/08012252" has no extension at all,
		// so without this the saved file would carry real HTML content
		// under a name that doesn't say so.
		if wantsHTML(exts) && extRe.FindString(name) == "" {
			name += ".html"
		}
		// Different sites routinely produce the same path basename (a
		// numeric ID, "index", ...) - silently overwriting file N with
		// file N+1 would look like a successful bulk download while
		// quietly losing most of it. Every saved name in this batch stays
		// unique, disambiguated by the source host first (still
		// meaningful to a human) and then by position if even that
		// collides.
		if usedNames[name] {
			host := ""
			if parsed != nil {
				host = unsafeName.ReplaceAllString(parsed.Host, "_")
			}
			ext := extRe.FindString(name)
			base := strings.TrimSuffix(name, ext)
			candidate := base + "_" + host + ext
			if usedNames[candidate] {
				candidate = fmt.Sprintf("%s_%d%s", base, i+1, ext)
			}
			name = candidate
		}
		usedNames[name] = true

		rel := filepath.ToSlash(filepath.Join(downloadDir, name))
		abs, err := rc.Workspace.ResolveSafe(rel)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %s", fileURL, err.Error()))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %s", fileURL, err.Error()))
			continue
		}
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %s", fileURL, err.Error()))
			continue
		}
		// Verified against the real file on disk, byte count included -
		// never assumed from a write call that returned no error.
		info, err := os.Stat(abs)
		if err != nil || info.Size() != int64(len(data)) {
			failed = append(failed, fmt.Sprintf("%s: write did not verify on disk", fileURL))
			continue
		}
		rc.Logf("download", "saved %s (%d bytes) - verified on disk", abs, info.Size())
		saved = append(saved, fmt.Sprintf("%s (%d bytes)", abs, info.Size()))
	}

	var b strings.Builder
	if len(saved) > 0 {
		fmt.Fprintf(&b, "Downloaded %d file(s) into %s/ (verified on disk):\n", len(saved), downloadDir)
		for _, s := range saved {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "Failed (%d):\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if leftOut > 0 {
		fmt.Fprintf(&b, "Not downloaded: %d more matching link(s) past the limit of %d per step.\n", leftOut, maxDownloads)
	}
	if b.Len() == 0 {
		b.WriteString("Nothing downloaded.")
	}

	// Verified only when every file asked for was saved and checked: 1 of 20
	// is not the download that was asked for.
	complete := len(saved) > 0 && len(failed) == 0 && leftOut == 0
	if !complete && len(saved) > 0 {
		fmt.Fprintf(&b, "Not complete: %d of %d file(s) saved.\n", len(saved), len(saved)+len(failed)+leftOut)
	}
	return Result{
		Subtask: subtask, SkillName: d.Name(),
		Output: b.String(), Verified: complete,
		Meta: map[string]any{"downloaded": len(saved), "failed": len(failed), "leftOut": leftOut, "paths": saved},
	}, nil
}

// givenURL is the URL step names, when it is one the work was given: the
// person's request or reply names it, or a verified earlier step actually
// fetched it. A URL only a plan's (a model's) wording carries is not taken -
// it may come from a model's summary or a page's injected text - and "" sends
// the skill to what earlier steps fetched instead. Outside a scheduler (no
// request recorded) the step is the request.
func givenURL(rc RunContext, step string) string {
	u := findURL(step)
	if u == "" || strings.TrimSpace(rc.OriginalPrompt) == "" {
		return u
	}
	if strings.Contains(rc.OriginalPrompt, u) || strings.Contains(rc.HumanReply, u) || slices.Contains(rc.PriorSourceURLs(), u) {
		return u
	}
	rc.Logf("url", "not using %s: neither the request nor a page fetched earlier gave it", u)
	return ""
}

// fileExt is the lower-case extension a URL's path names ("pdf"), or "".
func fileExt(fileURL string) string {
	if em := extRe.FindStringSubmatch(fileURL); em != nil {
		return strings.ToLower(em[1])
	}
	return ""
}

var (
	zipMagic = []byte("PK\x03\x04")
	oleMagic = []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1")
)

// contentMismatch says why data is not a file of kind ext - an HTML page (a
// login, an error) served under a .pdf name, a binary under a .csv - or ""
// when it is, or when ext is a kind this does not know.
func contentMismatch(ext string, data []byte) string {
	if len(data) == 0 {
		return "the server sent an empty file"
	}
	sniffed := http.DetectContentType(data)
	isHTML := strings.HasPrefix(sniffed, "text/html")
	isText := strings.HasPrefix(sniffed, "text/") || strings.HasPrefix(sniffed, "application/json")
	switch ext {
	case "pdf":
		// Readers accept the header anywhere in the first 1024 bytes.
		if !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF")) || isHTML {
			return "it is not a PDF (the server sent " + sniffed + ")"
		}
	case "xlsx", "docx", "pptx", "zip":
		if !bytes.HasPrefix(data, zipMagic) {
			return "it is not a ." + ext + " file (the server sent " + sniffed + ")"
		}
	case "xls", "doc", "ppt":
		// An old Office binary, or a newer zip saved under the old name.
		if !bytes.HasPrefix(data, oleMagic) && !bytes.HasPrefix(data, zipMagic) {
			return "it is not a ." + ext + " file (the server sent " + sniffed + ")"
		}
	case "json":
		if !json.Valid(data) {
			return "it is not valid JSON (the server sent " + sniffed + ")"
		}
	case "html", "htm":
		if !isText {
			return "it is not a web page (the server sent " + sniffed + ")"
		}
	case "csv", "cpp", "hpp", "h", "cc", "cxx", "py", "go", "rs", "java", "js", "ts":
		if isHTML || !isText {
			return "it is not a ." + ext + " text file (the server sent " + sniffed + ")"
		}
	}
	return ""
}
