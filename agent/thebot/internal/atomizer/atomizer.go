// Package atomizer decides task shape, in three layers that each cost more
// than the one before and are only reached when the cheaper one cannot
// decide:
//
//  1. Classify (this file) - deterministic regex classifiers that name the
//     one skill a step belongs to. "Never ask a model to judge its own task
//     granularity" was a hard-won rule in the Node prototype this replaces,
//     and it carries over.
//  2. Triage (triage.go) - still deterministic: whether a whole request is
//     conversation, one skill's job, a goal the operator should work out on
//     the machine by itself, or genuinely several different jobs.
//  3. Planner (plan.go) - a model call, made only for that last case (and
//     for recovery), that writes a tagged step graph the scheduler runs.
//
// The layer that used to sit where Triage is now was a model call asking
// "is this a task or a question?" for every request no classifier claimed.
// On the CPU-only hosts Decurix desktops run against, that single call took
// 60-100 seconds for "hello" - and on "what is the current kernel
// version?" it produced a one-step plan to search the web for the weather
// in New York.
package atomizer

import (
	"regexp"
	"strings"
)

type classifier struct {
	taskType string
	match    func(step string) bool
}

var registry []classifier

// Register adds one classifier to the bypass chain - order matters, the
// first match in Classify wins.
func Register(taskType string, match func(step string) bool) {
	registry = append(registry, classifier{taskType: taskType, match: match})
}

// Classify returns the first matching task type, or "general" if nothing
// in the chain claims this step.
func Classify(step string) string {
	for _, c := range registry {
		if c.match(step) {
			return withoutMissingBrowser(c.taskType)
		}
	}
	return "general"
}

// BrowserAvailable says whether the browser-agent extension is installed on
// this machine (soul sets it at startup). Without it, work in a web page is
// the operator's, driving the browser with gui: found live, a recovery plan
// sent "create the quotation in Odoo" to the browser skill on a desktop with
// no extension, which waited 30 s and failed the whole task.
var BrowserAvailable = true

// withoutMissingBrowser turns the browser task type into the operator's when
// the extension is not installed.
func withoutMissingBrowser(taskType string) string {
	if taskType == "browser" && !BrowserAvailable {
		return "operator"
	}
	return taskType
}

var (
	terminalRe = regexp.MustCompile(`(?i)\b(run|execute)\b[\s\S]*\b(script|command|terminal|shell|program|\.py|\.js|\.sh|\.ts)\b|\b(use|open) the terminal\b|\brun (it|that|this)\b`)
	feedbackRe = regexp.MustCompile(`(?i)\b(negative feedback|record (this )?feedback|remember that you|you (struggled|failed|are bad|are weak|are not good) (with|at))\b`)
	// The explicit-RAG phrasing, plus the natural way a person actually
	// asks "what do you already know about this" - "who is X", "what do
	// you know about X", "tell me about X". Live bug this second half
	// fixes: right after bulk-downloading and indexing 6 real pages about
	// a specific person, asking "who is X" in a brand new session did a
	// FRESH web search instead of checking what was already indexed -
	// and the fresh search pulled in details (a different employer, a
	// different degree) that never appeared in any of the verified
	// sources, suggesting it may have picked up a different same-named
	// person. Checking the Data RAG first means an already-researched
	// subject answers from verified, already-fetched content instead of
	// re-rolling the web search dice. If nothing is indexed, this fails
	// cheaply (no model call) and the scheduler's failure-recovery
	// correctly falls through to a real web search from there.
	ragQueryRe = regexp.MustCompile(`(?i)\b(data rag|the rag\b|rag index|indexed (files|documents)|search (the )?(rag|indexed)|from the rag)\b` +
		`|^\s*(please\s+)?(who is|what do you know about|tell me about|what can you tell me about)\b`)
	// "latest ... news/price/version/release" needs a gap allowed between
	// "latest" and the noun - live bug: "what's the latest AI news"
	// didn't match the old adjacent-words-only pattern (\blatest
	// (news|price...)\b), fell through to plain chat, and got "I don't
	// have real-time capabilities... I recommend checking a news
	// website" instead of the agent actually going and checking one
	// itself. "Latest news/updates/developments in/on/about X" is exactly
	// as real a signal that live web content is needed as "latest news"
	// alone was.
	webRe = regexp.MustCompile(`(?i)\b(search|google|look up|research|browse)\b[\s\S]*\b(web|internet|online|for)\b|\bsearch the web\b|\bon the internet\b|\bgo online\b|\blatest\b[\s\S]{0,25}?\b(news|prices?|version|release|updates?|developments?|stor(y|ies)|happenings?)\b|\bwhat('s| is|'re| are)\s+\S+\s+(up to|been up to)\b`)
	// Three real shapes: an explicit filename anywhere after a write verb
	// ("write a segment tree into segment_tree.py"), the words "file"/
	// "script" plus a destination ("save it to a file"), or a code/build
	// verb explicitly asking for multiple real files (see multiFileRe
	// below). The filename form matters most - it's how people actually
	// phrase it, and missing it sent "write X into foo.py" to plain chat,
	// which printed the code back instead of writing it. The multi-file
	// form fixes the same failure for "build a C++ codebase for a segment
	// tree, FFT, and a Fenwick tree, each in their own file" - no literal
	// filename anywhere, verb is "build" not "write/save/put", so neither
	// of the first two shapes matched and the whole codebase got dumped
	// as chat text instead of ever touching disk.
	fileWriteRe = regexp.MustCompile(`(?i)\b(write|create|save|generate|make|put)\b[\s\S]{0,120}?\b[\w-]+\.[a-z0-9]{1,8}\b` +
		`|\b(write|save|put)\b[\s\S]{0,60}?\b(in|into|to)\b[\s\S]{0,30}?\b(a |the )?(file|script)\b` +
		`|\b(build|implement|write|create|code)\b[\s\S]{0,200}?` + multiFilePattern)
	// Signals an explicit multi-file request regardless of which verb led
	// it - "each in their own file", "in separate files", "one file
	// each". Shared between fileWriteRe (classify as file-write at all)
	// and looksCompound (force real per-file planning instead of the
	// single-atomic-step bypass, which would silently drop every file but
	// the one the model picked as "the" file).
	multiFilePattern = `\beach in (its|their) own file|in separate files|in different files|one file each|as separate files|separate file for each\b`
	multiFileRe      = regexp.MustCompile(`(?i)` + multiFilePattern)
	downloadRe       = regexp.MustCompile(`(?i)\b(download|fetch|grab|pull)\b[\s\S]*\b(excel|xlsx?|csv|pdf|docx?|files?|documents?|dataset)\b`)
	urlOnlyRe        = regexp.MustCompile(`^\s*https?://\S+\s*$`)
	// A step that names a URL, or says to go to/visit a site, is web work -
	// whatever verb it used. Live failure: a planned step "Go to the
	// official Go website (https://golang.org/dl/)" matched nothing and
	// fell through to plain chat, which answered "I can't access web
	// pages". Anything pointing at a site belongs to the web skill, which
	// genuinely can fetch it.
	siteRe = regexp.MustCompile(`(?i)https?://\S+|\bwww\.\S+|\b(go to|visit|navigate to|open|check|read)\b[\s\S]{0,40}?\b(website|web page|webpage|site|url|link)\b`)

	// ------------------------------------------------------------ coding
	//
	// A request to WRITE CODE goes to the coding harness (internal/coding),
	// not to file-write. Both end up producing files, but only one of them
	// designs an architecture, scaffolds a real toolchain, writes tests,
	// RUNS them, and fixes the code until they pass. Handing real code to
	// file-write means shipping whatever a model emitted in one shot,
	// unexecuted - which is exactly how "here is your segment tree" ends up
	// being a file that doesn't compile.
	//
	// Four real shapes, kept separate so each can be tuned independently:
	//  1. a build verb aimed at a software NOUN ("build a REST API"),
	//  2. a build verb aimed at a language/framework or a real code file
	//     extension ("write a Go program", "implement quicksort in C++"),
	//  3. maintenance verbs on existing code ("fix the bug in", "refactor"),
	//  4. phrasing that leaves no doubt at all ("write me some code").
	codeNounPattern = `\b(app|application|program|script|package|library|module|` +
		`api|rest api|endpoint|server|service|microservice|backend|frontend|` +
		`cli|command[- ]line tool|tool|utility|dashboard|ui|website|web ?app|` +
		`bot|scraper|crawler|pipeline|parser|compiler|interpreter|game|` +
		`class|function|algorithm|implementation|data ?structure|codebase|` +
		`project|repo|repository|schema|test suite)\b`

	// "c++" and "c#" are deliberately OUTSIDE the \b(...)\b group: a
	// trailing \b after "+" never matches, because "+" is a non-word
	// character and so is whatever follows it (or end of string), so there
	// is no word boundary there at all. `\bc\+\+\b` therefore silently
	// fails to match "C++" - caught by this suite on "implement quicksort
	// in C++", which classified as "general" and would have been answered
	// in chat instead of built.
	codeLangPattern = `\bc\+\+|\bc#|` +
		`\b(python|go|golang|javascript|typescript|node(?:\.?js)?|` +
		`cpp|rust|java|ruby|php|sql|bash script|shell script|` +
		`fastapi|flask|django|streamlit|dash|dask|gradio|react|vue|svelte|express|gin|echo|` +
		`pandas|numpy|polars|pytorch|tensorflow|scikit-learn|sqlalchemy|pydantic|` +
		`sqlite|postgres(?:ql)?|mysql|mongodb|redis|kafka|celery|` +
		`docker|dockerfile|docker-compose|kubernetes|terraform|grpc|graphql|websockets?)\b`

	// The classic structures and algorithms people ask for by name. They
	// are unambiguously code even when the sentence names no language and
	// no file: "implement a trie and an AVL tree" is a build request in
	// every context it ever occurs in.
	codeThingPattern = `\b(linked ?list|hash ?(?:map|table|set)|binary (?:tree|search)|` +
		`avl tree|red[- ]black tree|b-?tree|trie|heap|stack|queue|graph|` +
		`segment tree|fenwick tree|suffix (?:tree|array)|bloom filter|lru cache|` +
		`quick ?sort|merge ?sort|heap ?sort|bubble ?sort|insertion sort|` +
		`dijkstra|a\*|bfs|dfs|knapsack|fibonacci|fft|matrix multiplication|` +
		`regex engine|state machine|rate limiter|connection pool|event loop)\b`

	codeExtPattern = `\.(?:py|go|js|jsx|mjs|ts|tsx|cpp|cc|cxx|hpp|rs|java|rb|php|cs|swift|kt|scala|sql)\b`

	codeVerbPattern = `\b(build|implement|write|create|code|develop|make|program|generate|design|set ?up|scaffold|port|migrate)\b`

	codingRe = regexp.MustCompile(`(?i)` +
		codeVerbPattern + `[\s\S]{0,80}?` + codeNounPattern +
		`|` + codeVerbPattern + `[\s\S]{0,80}?(?:` + codeLangPattern + `|` + codeExtPattern + `)` +
		`|` + codeVerbPattern + `[\s\S]{0,80}?` + codeThingPattern +
		// "implement" and "code (something)" are verbs that essentially
		// only ever mean software - unlike write/create/make/build, which
		// need a code-shaped object before they mean anything of the kind.
		`|\b(implement|code up)\b` +
		`|\b(fix|debug|refactor|optimi[sz]e|rewrite|extend)\b[\s\S]{0,60}?(?:` +
		codeNounPattern + `|` + codeExtPattern + `|` + codeThingPattern + `|\bcode\b|\bbug\b)` +
		`|\badd (?:a |some )?(?:unit |integration )?tests?\b` +
		`|\bwrite (?:me )?(?:some |the )?code\b`)

	// codingExcludeRe carves out requests that LOOK like code work but are
	// genuinely questions ABOUT code, not projects to build. Without it,
	// "explain how a segment tree works" would fire a whole
	// architect-scaffold-implement-test build, which is an absurd answer to
	// a question. Anchored at the start: these words only mean "this is a
	// question" when they LEAD the request - "build a CLI that explains
	// regexes" is still a real build.
	codingExcludeRe = regexp.MustCompile(`(?i)^\s*(please\s+)?(` +
		`explain|what(?:'s| is| are|'re)|how (?:do|does|can|would|should|did)|why\b|` +
		`tell me about|describe|compare|teach me|` +
		`show me an example|give me an example|when should i|which is better|` +
		`can you explain|help me understand|summari[sz]e|review\b|read\b|` +
		`who is|do you know` +
		`)\b`)
)

// codingOpsRe excludes steps whose real job is running a command or
// managing an environment, not building software. Both were live false
// positives found by this project's own test suite the moment the coding
// classifier was added:
//
//   - "run the command: python3 setup.py build" matched on build + .py,
//     and would have started a whole architect-scaffold-test build for
//     what is explicitly, unambiguously one shell command.
//   - "create a virtual environment, then install the package into it" -
//     a recovery step produced by the scheduler's own failure reasoning -
//     matched on create + package. It is environment work; the coding
//     harness would have tried to design a software project out of it.
var codingOpsRe = regexp.MustCompile(`(?i)^\s*(run|execute)\b[\s\S]{0,25}\b(command|script|shell|terminal)\b` +
	`|^\s*(create|make|set ?up|activate|delete|remove)\s+(a\s+|the\s+)?(new\s+)?(virtual\s?env|venv|virtual environment|environment|directory|folder)\b` +
	`|^\s*(pip|uv|npm|yarn|apt|apt-get|brew|cargo|gem)\b[\s\S]{0,15}\b(install|add|get|remove)\b` +
	`|^\s*install\b`)

var (
	// URLs are stripped before the coding patterns run. Real false
	// positive: "write a note about https://golang.org/dl/ to
	// go-version.md" classified as coding purely because the URL contains
	// "golang" - the language name was never about the note's content.
	codingURLRe = regexp.MustCompile(`https?://\S+|\bwww\.\S+`)

	// Filenames whose extension is NOT a code extension are stripped too,
	// for the second half of that same bug: "go-version.md" contains "go"
	// followed by a hyphen, which is a real word boundary, so \bgo\b
	// matched a MARKDOWN filename and made a note look like a Go project.
	// Removing non-code filenames wholesale fixes the whole class of it
	// (readme.md, notes.txt, data.csv) while leaving real code filenames
	// (fib.py, main.go) in place for codeExtPattern to find.
	// A lookahead would express this directly, but Go's RE2 has none.
	anyFilenameRe = regexp.MustCompile(`\b[\w-]+\.[a-zA-Z0-9]{1,8}\b`)
	codeExtOnlyRe = regexp.MustCompile(`(?i)` + codeExtPattern + `$`)
)

// stripNonCodeFilenames removes filenames that are not source files, so
// their stems can't be mistaken for language names.
func stripNonCodeFilenames(s string) string {
	return anyFilenameRe.ReplaceAllStringFunc(s, func(name string) string {
		if codeExtOnlyRe.MatchString(name) {
			return name
		}
		return " "
	})
}

// isCoding is the real gate for the coding harness: a genuine
// build-me-software request, never a question about software and never a
// single command or an environment chore.
func isCoding(step string) bool {
	if codingExcludeRe.MatchString(step) || codingOpsRe.MatchString(step) {
		return false
	}
	cleaned := stripNonCodeFilenames(codingURLRe.ReplaceAllString(step, " "))
	return codingRe.MatchString(cleaned)
}

// describesSoftware reports whether a step the planner tagged [code] names
// any software at all. A tagged step often has no verb ("a Flask app that
// serves the weather"), so one is supplied - but only after the question
// and chore exclusions have had their say on the step as written.
func describesSoftware(step string) bool {
	if codingExcludeRe.MatchString(step) || codingOpsRe.MatchString(step) {
		return false
	}
	cleaned := stripNonCodeFilenames(codingURLRe.ReplaceAllString(step, " "))
	return codingRe.MatchString(cleaned) || codingRe.MatchString("build "+cleaned)
}

func init() {
	// Order matters (first match wins): the specific, unambiguous shapes
	// go before broader ones. feedback/rag-query phrasing is distinctive
	// enough to lead; terminal's pattern is narrow; web and file-write
	// come before the general fallback.
	Register("feedback", feedbackRe.MatchString)
	Register("rag-query", ragQueryRe.MatchString)
	// coding goes BEFORE download, terminal and file-write, all three of
	// which would otherwise claim real coding requests and do something
	// much weaker with them. "write a python script that downloads the CSV
	// files" matches download (download + csv) and would have been handled
	// as a fetch; "write a segment tree to segment_tree.py" matches
	// file-write and would have been one unexecuted model completion saved
	// to disk. Both are really "build me software", and only the coding
	// harness actually builds, tests and fixes it.
	Register("coding", isCoding)
	Register("download", downloadRe.MatchString)
	Register("terminal", terminalRe.MatchString)
	Register("file-write", fileWriteRe.MatchString)
	// browser before web: a step that names a destination AND an action on
	// it ("search google.com for X") is interactive browsing, not a fetch -
	// see wantsBrowser in triage.go for the same distinction at the whole-
	// request level.
	Register("browser", wantsBrowser)
	Register("web", func(step string) bool {
		return webRe.MatchString(step) || urlOnlyRe.MatchString(step) || siteRe.MatchString(step)
	})
}

// noOpStepRe catches planned steps that ask for nothing a skill can do -
// leftovers from planning as if for a human ("Save the file.", "Open a
// text editor."). Executing them wastes a model call and, worse, invites
// the "I can't do that" answer that made the whole run look broken.
// Writing a file already saves it; there is nothing to open.
// The whole step must be the no-op. A trailing ".*" here would swallow
// steps that merely start with dead phrasing but still carry real work -
// "Open a text editor and create a new file named go-version.md" names a
// file, so it is a genuine file-write and must survive.
var noOpStepRe = regexp.MustCompile(`(?i)^\s*(` +
	`save( the| it| this)?( file| it| changes)?` +
	`|open (a |the )?(text )?editor` +
	`|close (the )?(file|editor)` +
	`|verify (the |your )?(file|result|results|output|contents?)` +
	`|review (the |your )?(result|results|output)` +
	`|report( back)?( the)?( result| results| findings)?( to the user)?` +
	`|done|finish(ed)?` +
	`|repeat (steps?|this)\b[\s\S]*` +
	`)\s*\.?\s*$`)

// dropNoOpSteps removes steps that cannot map to any real capability.
// A plan that becomes empty is not a task at all.
func dropNoOpSteps(steps []string) []string {
	out := steps[:0:0]
	for _, s := range steps {
		if noOpStepRe.MatchString(s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// actionVerbRe counts the distinct things a prompt asks for. Two or more
// means it needs a real plan, not a single skill.
var (
	actionVerbRe = regexp.MustCompile(`(?i)\b(search|google|look up|research|browse|find out|check|download|fetch|write|create|save|generate|run|execute|install|build|test|summari[sz]e|compare|analyse|analyze)\b`)
	// A conditional fallback ("if you don't know X, search for it") is as
	// real a joiner as "and"/"then" - it just doesn't look like one. Live
	// bug: "write the fastest C++ implementation of X... if you don't
	// know one, search the web to find it" has two distinct verbs
	// (write, search) but no and/then joiner, so looksCompound() missed
	// it, the single-atomic "web" bypass swallowed the WHOLE request
	// (including the file-write half) as one research-and-answer-in-text
	// step, and genuinely excellent research (it found the real 2025 SSSP
	// breakthrough) still ended with the code dumped into chat instead of
	// a file.
	joinerRe = regexp.MustCompile(`(?i)\b(and then|then|after that|and also|, then|and save|and write|and store|and put|and run|and download|and summari[sz]e|if you (don't|do not|aren't|are not) (know|sure|certain))\b`)

	// writeVerbs all mean "put content in a file", and folderRe is the one
	// thing that makes "create" mean something else.
	writeVerbs = map[string]bool{"write": true, "create": true, "save": true, "generate": true}
	folderRe   = regexp.MustCompile(`(?i)\b(folders?|director(y|ies))\b`)
)

// namesOneFile reports whether a prompt is about exactly one file and no
// folder, which is when its write verbs are all the same job.
func namesOneFile(prompt string) bool {
	if folderRe.MatchString(prompt) {
		return false
	}
	files := map[string]bool{}
	for _, f := range anyFilenameRe.FindAllString(prompt, -1) {
		files[strings.ToLower(f)] = true
	}
	return len(files) == 1
}

// looksCompound reports whether a prompt asks for more than one thing.
// Two different action verbs joined by "and"/"then" is the real signal -
// a single action repeated ("write and save the file") is not compound,
// which is why the joiner has to be present too.
//
// An explicit multi-file request is compound even with a SINGLE verb,
// which is why it's checked separately rather than folded into the
// verb-counting rule: "build a codebase for X, Y, and Z, each in their
// own file" uses one verb ("build") for three components, so the
// verb-counting rule alone would call it non-compound - and the
// single-atomic-step bypass would then hand the whole request to
// file-write as ONE write, silently producing only one of the three
// files the model happened to pick.
//
// Different write verbs about one file are also one action. Measured live:
// "create a file koustubh.txt and write a poem inside it" counted create
// and write as two jobs, spent two minutes planning on a CPU host, and the
// planner tagged "create a new file named koustubh.txt" as [code] - a whole
// build harness for an empty text file, which timed out.
func looksCompound(prompt string) bool {
	if multiFileRe.MatchString(prompt) {
		return true
	}
	if !joinerRe.MatchString(prompt) {
		return false
	}
	oneFile := namesOneFile(prompt)
	seen := map[string]bool{}
	for _, m := range actionVerbRe.FindAllString(prompt, -1) {
		verb := strings.ToLower(m)
		if oneFile && writeVerbs[verb] {
			verb = "write"
		}
		seen[verb] = true
	}
	return len(seen) >= 2
}
