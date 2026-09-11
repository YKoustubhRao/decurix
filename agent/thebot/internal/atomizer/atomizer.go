// Package atomizer decides task shape. Deterministic regex classifiers
// run FIRST, before any model call - "never ask a model to judge its own
// task granularity" was a hard-won rule in the Node prototype this
// replaces, and it carries over. Only when no classifier claims a prompt
// does a real model call get made, to decide whether it's a multi-step
// task worth planning or just something to answer directly.
package atomizer

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"thebot/internal/llm"
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
			return c.taskType
		}
	}
	return "general"
}

// Plan is what Atomize produces: the real steps to run, and whether this
// was judged an actual task at all.
type Plan struct {
	Steps  []string
	IsTask bool
	Reason string
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
)

func init() {
	// Order matters (first match wins): the specific, unambiguous shapes
	// go before broader ones. feedback/rag-query phrasing is distinctive
	// enough to lead; terminal's pattern is narrow; web and file-write
	// come before the general fallback.
	Register("feedback", feedbackRe.MatchString)
	Register("rag-query", ragQueryRe.MatchString)
	Register("download", downloadRe.MatchString)
	Register("terminal", terminalRe.MatchString)
	Register("file-write", fileWriteRe.MatchString)
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
)

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
func looksCompound(prompt string) bool {
	if multiFileRe.MatchString(prompt) {
		return true
	}
	if !joinerRe.MatchString(prompt) {
		return false
	}
	seen := map[string]bool{}
	for _, m := range actionVerbRe.FindAllString(prompt, -1) {
		seen[strings.ToLower(m)] = true
	}
	return len(seen) >= 2
}

// planPrompt is deliberately skill-aware. A generic "break this into
// steps" prompt plans for a HUMAN at a desktop - the live failure was
// "find out the newest Go version and save a note" becoming "Go to the
// official Go website", "Open a text editor", "Save the file". None of
// those map to a skill, so all three fell through to plain chat, which
// answered "I can't access web pages" and wrote nothing useful. Telling
// the planner exactly which capabilities exist, and how a step must be
// phrased to reach each one, is what keeps the plan executable.
const planPrompt = `You are planning work for an agent with a fixed set of capabilities. It is NOT a human at a desktop.

The agent can ONLY do these things. Every step you write must be one of them, phrased so it is obviously that capability:
- SEARCH THE WEB - phrase as "search the web for X". Use this for anything current, live, or that you are not certain of. The agent really does fetch and read pages.
- WRITE A FILE - phrase as "write <what> to <filename>". This creates the file AND saves it in one step.
- RUN A COMMAND - phrase as "run the command: <cmd>".
- DOWNLOAD FILES - phrase as "download the <type> files from <url>".
- SEARCH INDEXED DOCUMENTS - phrase as "search the indexed documents for X".
- ANSWER FROM KNOWLEDGE - only for things that need no tool at all.

Rules:
- NEVER write GUI steps ("open a browser", "go to the website", "open a text editor", "click", "save the file"). The agent has no hands.
- NEVER split creating and saving a file into two steps - writing a file saves it.
- Do not add steps for verifying, reviewing, or reporting back. That happens automatically.
- NEVER write a step that tells the agent to repeat, retry, or loop other steps ("repeat steps 4-6 until it works"). There is no step executor for a loop instruction - write out each real attempt as its own concrete step instead, or leave retrying to the agent's own real failure-recovery, which already exists.
- Use the FEWEST steps that actually do the job, at most 10. Two is usually right for "find out X and write it to Y"; a request for several separate items (each in its own file) legitimately needs up to 2 steps per item.

If the request is a simple question, greeting, or something answerable directly in conversation with no tool, respond exactly:
TASK: no

Otherwise respond:
TASK: yes
STEPS:
1. <first step, phrased as one of the capabilities above>
2. <second step>

Example
Request: Find out what the newest Go version is and save a short note about it to go-version.md
TASK: yes
STEPS:
1. search the web for the newest Go version
2. write a short note about the newest Go version to go-version.md

Request: %s`

// stepsOnlyPrompt is used instead of planPrompt when a deterministic
// classifier already matched a real action verb in the request (so this
// is compound, not unclassifiable) - it asks ONLY for the step breakdown,
// never "is this a task at all".
//
// Real bug this fixes: even at a low, near-deterministic temperature, the
// full planPrompt (which re-asks task-vs-question every time) kept landing
// on "TASK: no" for "...download the raw html of all 10 pages using a
// browser" - not because of sampling noise, but because that IS the
// model's single most likely completion for phrasing that pattern-matches
// its trained refusal habits around scraping/downloading. Lowering
// temperature made that WRONG judgment fire consistently instead of
// intermittently; it never made it right. The actual fix is architectural,
// the same principle the classifier bypass already rests on: never ask a
// model to re-judge something already established by other means. A
// classifier matching "download" or "search the web" already proves this
// is real, permitted work - the model should never be given the chance to
// disagree with that, only to say how many steps it takes.
const stepsOnlyPrompt = `Break this request into the concrete steps needed. This IS real, permitted work for an agent with tools - that is already established, not a question you get to reconsider. Never refuse, question, or add a disclaimer about permissions/terms of service - just plan the steps.

The agent can ONLY do these things. Every step you write must be one of them, phrased so it is obviously that capability:
- SEARCH THE WEB - phrase as "search the web for X". The agent really does fetch and read pages.
- WRITE A FILE - phrase as "write <what> to <filename>". This creates the file AND saves it in one step.
- RUN A COMMAND - phrase as "run the command: <cmd>".
- DOWNLOAD FILES - phrase as "download the <type> files from <url>" (or "from the search results" if there's no single URL). The agent really does fetch and save real files, including raw HTML pages.
- SEARCH INDEXED DOCUMENTS - phrase as "search the indexed documents for X".

Rules:
- NEVER write GUI steps ("open a browser", "go to the website", "click"). The agent has no hands - it fetches things directly over HTTP, which is what "using a browser" in a request means here.
- NEVER split creating/downloading and saving into two steps - each capability above already saves what it produces.
- A request for SEVERAL named files (each in its own file, one file each, separate files, a codebase with multiple components) needs ONE "write ... to <filename>" step PER FILE - never one step trying to cover several files at once, and never asked as a plain question. Each file needs its own concrete filename. If you aren't confident in one of them, add a "search the web for X" step right before writing that file.
- NEVER write a step that tells the agent to repeat, retry, or loop other steps ("repeat steps 4-6 until it works"). There is no step executor for a loop instruction - write out each real attempt as its own concrete step, or leave retrying to the agent's own real failure-recovery, which already exists.
- Use the FEWEST steps that actually do the job, at most 10.

Respond in EXACTLY this format, nothing else:
STEPS:
1. <first step, phrased as one of the capabilities above>
2. <second step>

Example
Request: search the web for X and download the raw html of the top results using a browser
STEPS:
1. search the web for X
2. download the raw html files from the search results

Example
Request: build a C++ codebase using templates for a segment tree and an FFT, each in their own file
STEPS:
1. write a C++ segment tree template to segment_tree.h
2. write a C++ FFT (fast fourier transform) implementation to fft.h

Request: %s`

var stepLineRe = regexp.MustCompile(`^\s*\d+[.)]\s*(.+)$`)

// maxSteps caps how large a single plan can get. Raised from 5 to 10
// after a live failure: "write C++ implementations of X, Y, and Z, each
// in its own file, searching the web first for any you don't know well"
// genuinely needs up to 2 steps per item (search, then write) - 3 items
// is already 6, past the old cap. The cap silently truncated to 5,
// dropping the LAST write step with no signal anywhere that anything was
// cut, so the final item's code came out in chat instead of a file - the
// dropped step's search result still ran and included example code in
// its own prose, which is what actually leaked into the answer.
const maxSteps = 10

// parseSteps pulls numbered step lines out of a model response, drops any
// that don't map to a real capability, and logs what was dropped -
// including when the cap itself has to cut real, valid steps, which used
// to happen in total silence.
func parseSteps(text string, onLog func(string)) []string {
	var steps []string
	for _, line := range strings.Split(text, "\n") {
		if m := stepLineRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			steps = append(steps, strings.TrimSpace(m[1]))
		}
	}
	before := len(steps)
	steps = dropNoOpSteps(steps)
	if n := before - len(steps); n > 0 && onLog != nil {
		onLog(fmt.Sprintf("atomizer: dropped %d step(s) that no skill can act on", n))
	}
	if len(steps) > maxSteps {
		if onLog != nil {
			onLog(fmt.Sprintf("atomizer: plan had %d real step(s), capping at %d - the request may need to be split up", len(steps), maxSteps))
		}
		steps = steps[:maxSteps]
	}
	return steps
}

// Atomize decides whether a prompt is a real multi-step task and, if so,
// what the steps are.
//
// A prompt that a deterministic classifier already claims is left as one
// atomic step - it maps directly to a skill and needs no planning, and
// asking a model to re-plan it only adds a failure mode. Everything else
// gets one real model call to decide task-vs-chat.
func Atomize(ctx context.Context, prompt string, provider llm.Provider, model string, onLog func(string)) Plan {
	matched := Classify(prompt)

	// The single-step bypass only applies to a SINGLE clear action. Found
	// live: "find out the newest Go version AND save a note to
	// go-version.md" matched file-write and got run as one atomic step,
	// so it never searched anything - it wrote the file straight from the
	// model's stale knowledge. A compound request has to go to the
	// planner even when a classifier would happily claim it.
	if matched != "general" && !looksCompound(prompt) {
		if onLog != nil {
			onLog(fmt.Sprintf("atomizer: recognised this as a %q task - one atomic step, no planning needed", matched))
		}
		return Plan{Steps: []string{prompt}, IsTask: true, Reason: "matched the " + matched + " classifier"}
	}

	if provider == nil {
		reason := "no provider available to plan with"
		// A classifier already proved this is real work, even without a
		// provider to break it into steps - run it as one atomic step
		// rather than downgrading to "just a question".
		if matched != "general" {
			return Plan{Steps: []string{prompt}, IsTask: true, Reason: reason + " (ran as one step - " + matched + " already matched)"}
		}
		return Plan{Steps: []string{prompt}, IsTask: false, Reason: reason}
	}

	// A classifier matching a real action verb already proves this is
	// permitted, real work - the model is asked ONLY how many steps it
	// takes, never whether it's a task at all. See stepsOnlyPrompt's
	// comment for why this split exists: re-asking task-vs-question at
	// this point let the model land on "TASK: no" for "...download the
	// raw html of all 10 pages using a browser" CONSISTENTLY even at a
	// near-deterministic temperature - not noise, its genuine most-likely
	// completion for phrasing that resembles a scraping request it would
	// normally refuse. Removing the chance to disagree removes the bug.
	if matched != "general" {
		if onLog != nil {
			onLog(fmt.Sprintf("atomizer: recognised this as %q, but it asks for more than one thing - planning multiple steps...", matched))
		}
		resp, err := provider.Chat(ctx, model, []llm.Message{{Role: "user", Content: fmt.Sprintf(stepsOnlyPrompt, prompt)}}, llm.Options{TimeoutMs: 60000, Temperature: 0.15})
		if err != nil {
			if onLog != nil {
				onLog("atomizer: step-planning call failed (" + err.Error() + ") - running it as one atomic step instead")
			}
			return Plan{Steps: []string{prompt}, IsTask: true, Reason: "step-planning call failed, matched the " + matched + " classifier"}
		}
		steps := parseSteps(resp.Text, onLog)
		if len(steps) == 0 {
			// The model produced nothing usable - degrade to one atomic
			// step rather than a failed task. We already KNOW this is
			// real work; a bad planning response can't undo that.
			if onLog != nil {
				onLog("atomizer: could not parse a step breakdown - running it as one atomic step instead")
			}
			return Plan{Steps: []string{prompt}, IsTask: true, Reason: "step-planning produced nothing usable, matched the " + matched + " classifier"}
		}
		if onLog != nil {
			onLog(fmt.Sprintf("atomizer: planned %d step(s):", len(steps)))
			for i, s := range steps {
				onLog(fmt.Sprintf("atomizer:   %d. %s", i+1, s))
			}
		}
		return Plan{Steps: steps, IsTask: true, Reason: "judged a multi-step task (" + matched + ")"}
	}

	// Nothing matched at all - this is the one case that genuinely needs
	// a task-vs-question judgment, since there is no other signal.
	if onLog != nil {
		onLog("atomizer: no classifier claimed this - deciding whether it's a task or a question...")
	}

	// Low temperature: this is a structured yes/no classification, not
	// creative writing. Live bug: the identical prompt sent twice at the
	// provider's default temperature came back "TASK: yes" once and
	// "TASK: no" the next time.
	resp, err := provider.Chat(ctx, model, []llm.Message{{Role: "user", Content: fmt.Sprintf(planPrompt, prompt)}}, llm.Options{TimeoutMs: 60000, Temperature: 0.15})
	if err != nil {
		// Failing to plan must never block answering - fall back to
		// treating it as a plain question rather than erroring out.
		if onLog != nil {
			onLog("atomizer: planning call failed (" + err.Error() + ") - treating it as a question")
		}
		return Plan{Steps: []string{prompt}, IsTask: false, Reason: "planning call failed"}
	}

	isTask := false
	for _, line := range strings.Split(resp.Text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "TASK:") {
			isTask = strings.Contains(strings.ToLower(trimmed), "yes")
			break
		}
	}
	steps := parseSteps(resp.Text, onLog)

	if !isTask || len(steps) == 0 {
		if onLog != nil {
			onLog("atomizer: this is a question, not a task - answering directly")
		}
		return Plan{Steps: []string{prompt}, IsTask: false, Reason: "judged a plain question"}
	}

	if onLog != nil {
		onLog(fmt.Sprintf("atomizer: this is a task - planned %d step(s):", len(steps)))
		for i, s := range steps {
			onLog(fmt.Sprintf("atomizer:   %d. %s", i+1, s))
		}
	}
	return Plan{Steps: steps, IsTask: true, Reason: "judged a multi-step task"}
}
