package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"thebot/internal/llm"
	"thebot/internal/websearch"
)

// WebSkill does real web research: a real search against real engines,
// a real deep-read of the top hits (not just their snippets), then a
// grounded answer that cites the actual URLs it used. The report is saved
// into the workspace so the work is reusable rather than thrown away
// after one answer.
//
// Ported back from the Node prototype's skills/web.js after the Go
// rewrite shipped without it - "it used to actually open the web" was a
// real capability regression, not a misremembering.
type WebSkill struct{}

func NewWeb() *WebSkill { return &WebSkill{} }

func (w *WebSkill) Name() string { return "web" }
func (w *WebSkill) Description() string {
	return "Searches the real web, deep-reads the top results, and answers with real citations."
}

func (w *WebSkill) Matches(subtask Subtask) bool { return subtask.Type == "web" }

const reportDir = "web-reports"

// The deep-read budget: how many of the top results are read in full, and
// how much of each. Everything read goes into one synthesis prompt, and on
// a CPU-only host reading that prompt IS the cost - three pages of 6000
// characters is ~5500 tokens, which a 3B model on an ordinary laptop took
// more than six minutes to get through before writing a word. A GPU reads
// the same prompt in seconds, so the defaults stay generous here and a host
// that knows it is CPU-bound turns them down (Decurix's start script does).
var (
	deepReadCount    = envPositive("THEBOT_WEB_PAGES", 3)
	deepReadMaxChars = envPositive("THEBOT_WEB_PAGE_CHARS", 6000)
)

func envPositive(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return fallback
}

var (
	urlRe = regexp.MustCompile(`https?://[^\s)]+`)
	// "for" gets its own optional group AFTER the web/internet group.
	// Folding it into that alternation left the word behind on the most
	// common phrasing of all: "search the web for X" consumed "web" and
	// searched for "for X", sending a stray preposition to the engine on
	// every single search.
	searchLeadRe = regexp.MustCompile(`(?i)^\s*(please\s+)?(can you\s+)?(go\s+)?(search|google|look\s*up|find\s+out|find|research|browse)\s+(the\s+)?(web|internet|online)?\s*(for\s+)?`)
	slugRe       = regexp.MustCompile(`[^a-z0-9]+`)
)

// extractQuery turns "search the web for X" into "X" - the engines want
// the subject, not the instruction wrapped around it.
func extractQuery(step string) string {
	q := searchLeadRe.ReplaceAllString(step, "")
	q = strings.TrimSpace(strings.Trim(q, `"'`))
	if q == "" {
		return strings.TrimSpace(step)
	}
	return q
}

func (w *WebSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	// A bare URL means "read this page", not "search for it".
	if m := urlRe.FindString(subtask.Step); m != "" && strings.Contains(strings.ToLower(subtask.Step), "http") {
		return w.readPage(rc, subtask, m)
	}

	query := extractQuery(subtask.Step)
	rc.Logf("web", "searching the real web for: %s", query)

	results, err := websearch.Search(rc.Ctx, query, rc.OnLog)
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: w.Name(),
			Output:   "Web search failed: " + err.Error(),
			Verified: false, Meta: map[string]any{"error": err.Error(), "query": query},
		}, nil
	}

	// Deep-read the top hits: real page text, not just the engine's
	// snippet, so the answer is grounded in what the pages actually say.
	type readPage struct {
		result websearch.Result
		text   string
		err    error
	}
	var reads []readPage
	for i, r := range results {
		if i >= deepReadCount {
			break
		}
		rc.Logf("web", "reading page %d/%d: %s", i+1, min(deepReadCount, len(results)), r.URL)
		text, err := websearch.FetchPageText(rc.Ctx, r.URL, deepReadMaxChars)
		reads = append(reads, readPage{result: r, text: text, err: err})
		if err != nil {
			rc.Logf("web", "could not read %s: %s", r.URL, err.Error())
		}
	}

	var context strings.Builder
	context.WriteString("Search results:\n")
	for i, r := range results {
		if i >= 8 {
			break
		}
		fmt.Fprintf(&context, "%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	context.WriteString("\nFull page content from the top results:\n")
	for _, rd := range reads {
		if rd.err != nil {
			fmt.Fprintf(&context, "\n--- %s ---\n[could not fetch: %s]\n", rd.result.URL, rd.err.Error())
			continue
		}
		fmt.Fprintf(&context, "\n--- %s ---\n%s\n", rd.result.URL, rd.text)
	}

	// Found live: asked to "download the raw html... using a browser", the
	// model fetched the pages successfully (real content is right below in
	// this same prompt) and then answered "I'm unable to perform web
	// scraping... as per my guidelines" - a refusal reflex triggered by the
	// REQUEST's wording, ignoring that the fetching already happened. The
	// content below is real; the framing has to say so explicitly or the
	// model pattern-matches on "scrape/browse/download" and refuses on
	// principle regardless of what it was actually just given.
	prompt := fmt.Sprintf(
		"You already searched the real web and fetched the real page content below with your own tools - this "+
			"already happened, successfully, before this message. Never say you can't browse, scrape, or access web "+
			"pages: you just did, and everything below is real. Do not add a disclaimer about being unable to fetch "+
			"web content - that would be false; report what was actually found instead.\n\n"+
			"Answer the user's request using ONLY the real search results and page content below. "+
			"Cite the actual URLs you used. If the sources don't answer it, say so plainly instead of guessing.\n\n"+
			"Request: %s\n\n%s",
		subtask.Step, context.String())

	rc.Logf("web", "synthesizing an answer from %d result(s) and %d deep-read page(s)", len(results), len(reads))
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{rc.SystemMessage(), {Role: "user", Content: prompt}}, llm.Options{TimeoutMs: 180000})
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: w.Name(),
			Output:   fmt.Sprintf("Found %d real results but could not compose an answer: %s", len(results), err.Error()),
			Verified: false, Meta: map[string]any{"error": err.Error(), "results": len(results)},
		}, nil
	}

	// Save the real report so the research is reusable - it also gets
	// picked up by the scheduler's P0 RAG ingestion automatically.
	reportPath := w.saveReport(rc, query, subtask.Step, resp.Text, results)

	sources := make([]string, 0, len(results))
	for i, r := range results {
		if i >= 8 {
			break
		}
		sources = append(sources, r.URL)
	}

	out := stripControlMarker(resp.Text)
	if reportPath != "" {
		out += "\n\nSaved the full research report to: " + reportPath
	}

	return Result{
		Subtask: subtask, SkillName: w.Name(),
		Output: out, Verified: true,
		Meta: map[string]any{"query": query, "results": len(results), "sources": sources, "path": reportPath, "userFacing": true},
	}, nil
}

func (w *WebSkill) readPage(rc RunContext, subtask Subtask, target string) (Result, error) {
	rc.Logf("web", "fetching page: %s", target)
	text, err := websearch.FetchPageText(rc.Ctx, target, deepReadMaxChars*2)
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: w.Name(),
			Output:   "Could not fetch " + target + ": " + err.Error(),
			Verified: false, Meta: map[string]any{"error": err.Error(), "url": target},
		}, nil
	}

	prompt := fmt.Sprintf(
		"You already fetched this real page with your own tools, successfully - never say you can't access web "+
			"pages, you just did. Answer using ONLY this real page content from %s.\n\nRequest: %s\n\nPage content:\n%s",
		target, subtask.Step, text)
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{rc.SystemMessage(), {Role: "user", Content: prompt}}, llm.Options{TimeoutMs: 180000})
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: w.Name(),
			Output: "Fetched the page but could not compose an answer: " + err.Error(), Verified: false,
			Meta: map[string]any{"error": err.Error(), "url": target},
		}, nil
	}
	return Result{
		Subtask: subtask, SkillName: w.Name(),
		Output: stripControlMarker(resp.Text), Verified: true, Meta: map[string]any{"url": target, "userFacing": true},
	}, nil
}

func (w *WebSkill) saveReport(rc RunContext, query, request, answer string, results []websearch.Result) string {
	if rc.Workspace == nil {
		return ""
	}
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(query), "-"), "-")
	if len(slug) > 60 {
		slug = slug[:60]
	}
	if slug == "" {
		slug = "search"
	}
	rel := filepath.ToSlash(filepath.Join(reportDir, fmt.Sprintf("%s-%d.md", slug, time.Now().Unix())))
	abs, err := rc.Workspace.ResolveSafe(rel)
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Web research: %s\n\n_Request: %s_\n\n_Searched: %s_\n\n## Answer\n\n%s\n\n## Sources\n\n",
		query, request, time.Now().Format(time.RFC3339), answer)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. [%s](%s)\n   > %s\n\n", i+1, r.Title, r.URL, r.Snippet)
	}

	if err := os.WriteFile(abs, []byte(b.String()), 0o644); err != nil {
		return ""
	}
	rc.Logf("web", "saved report: %s", abs)
	return abs
}

// stripControlMarker removes an approval marker a research answer opened
// with. Research runs nothing and needs no approval, but the host's approval
// rules sit in the same system frame, and a 3B model opened a plain summary
// of Go's release notes with "NEEDS_APPROVAL:" - which, reaching the host as
// the job's answer, would page a human to approve a paragraph.
func stripControlMarker(text string) string {
	t := strings.TrimSpace(text)
	for _, marker := range []string{"NEEDS_APPROVAL:", "HELP:", "DONE:"} {
		if strings.HasPrefix(t, marker) {
			return strings.TrimSpace(t[len(marker):])
		}
	}
	return t
}
