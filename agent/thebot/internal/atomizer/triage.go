package atomizer

import (
	"fmt"
	"regexp"
	"strings"
)

// RouteKind is the shape of a whole request.
type RouteKind string

const (
	// RouteChat is conversation: a greeting, a question about TheBot or
	// the user, general knowledge. One model call, no tools. If that call
	// discovers it needs to look at the machine after all, it says so
	// (prompt.NeedsToolsMarker) and the scheduler reroutes it to the
	// operator - so a miss here costs one short call, never a wrong answer.
	RouteChat RouteKind = "chat"
	// RouteSkill is exactly one classified skill's job, run as one step.
	RouteSkill RouteKind = "skill"
	// RouteOperate is one goal on this machine that the operator skill
	// works out command by command ("check the disk space", "how many
	// files are in /etc?"). It deliberately skips planning: the operator's
	// own loop already decides the next command from the last one's
	// output, which a plan written up front cannot.
	RouteOperate RouteKind = "operate"
	// RoutePlan is several genuinely different jobs ("find out X on the web
	// and write it to Y"). The only route that asks a model to plan.
	RoutePlan RouteKind = "plan"
	// RouteBrowser is one interactive session in a real, live web page -
	// "Open google.com. Search for this. Do that." - handed whole to the
	// browser skill, which drives it through the browser-agent Chrome
	// extension (agent/browser-agent/extension) rather than the operator's
	// coordinate/xdotool loop. Like RouteOperate, it deliberately skips
	// planning: the extension's own atomize -> act -> verify loop already
	// decides its next action from what the last one actually did to the
	// page, which a plan written before the first page load cannot.
	RouteBrowser RouteKind = "browser"
)

// Route is Triage's decision, with the reason it gives in the live log.
type Route struct {
	Kind RouteKind
	// TaskType is the classifier that matched the whole request, or
	// "general" when none did.
	TaskType string
	Reason   string
}

var (
	// Short social turns. Anchored and length-limited: "hello" is chat,
	// "hello, please install jq" is not.
	greetingRe = regexp.MustCompile(`(?i)^\s*(hi+|hello+|hey+|yo|hiya|howdy|greetings|good\s+(morning|afternoon|evening|night)|thanks?|thank\s+you|thx|ty|cheers|cool|great|nice|awesome|perfect|ok(ay)?|got\s+it|bye|goodbye|see\s+you|how\s+are\s+you|how's\s+it\s+going|what's\s+up|sup)\b[\s\S]{0,40}$`)

	// Questions about TheBot itself or about what it knows of the user.
	// Answering these needs the system frame (identity, memory), never a
	// tool - and asking the operator would have it run `hostname` to learn
	// its own name.
	selfRe = regexp.MustCompile(`(?i)\b(your\s+(\w+\s+)?name|who\s+are\s+you|what\s+are\s+you|what\s+(can|do)\s+you\s+do|how\s+do\s+you\s+work|your\s+(capabilities|skills|abilities|purpose)|introduce\s+yourself|are\s+you\s+(a|an)\s+(bot|ai|human|robot|person)|which\s+model|what\s+model|what('s|\s+is)\s+my\s+name|who\s+am\s+i|what\s+do\s+you\s+(know|remember)\s+about\s+me|do\s+you\s+remember)\b`)

	// Telling TheBot something about the user, which L1 records afterwards.
	statementRe = regexp.MustCompile(`(?i)^\s*(my\s+name\s+is|call\s+me|i\s+am\s+(called|named)|i'm\s+(called|named)|remember\s+(that|this)|please\s+remember|note\s+that\s+i|i\s+(prefer|like|love|hate|work\s+(at|for|as)|live\s+in))\b`)

	// Something on this machine is involved: a path, a file, or a noun
	// that only means machine state. Question form does not matter - "how
	// many files are in /etc?" is a question and has exactly one honest
	// answer, the one `ls` gives.
	machinePathRe = regexp.MustCompile(`(^|[\s"'(=:])(~/|~$|/[a-zA-Z_.])|\b[\w-]+\.(txt|md|csv|tsv|json|jsonl|log|conf|cfg|ini|ya?ml|toml|xml|html?|py|js|ts|go|rs|sh|sql|pdf|docx?|xlsx?|pptx?|odt|ods|png|jpe?g|gif|svg|zip|tar|gz|tgz|deb|iso)\b`)
	machineNounRe = regexp.MustCompile(`(?i)\b(files?|folders?|director(y|ies)|disk(\s+space)?|free\s+space|storage|partitions?|memory\s+usage|ram|swap|cpus?|processor|cores|gpus?|process(es)?|pids?|services?|daemons?|ports?|listening|network\s+interfaces?|ip\s+address(es)?|hostname|kernel|operating\s+system|distro|uptime|load\s+average|environment\s+variables?|env\s+vars?|installed|uninstall(ed)?|upgrad(e|ed|able)|packages?|apt|pip|npm|cron(tab)?|systemd|containers?|logged\s+in|user\s+accounts?|permissions?|owner(ship)?|symlinks?|clipboard|screenshot|wallpaper|downloads\s+folder|home\s+(folder|directory)|work(ing)?\s+(folder|directory)|workspace|this\s+(machine|computer|system|box|vm|desktop|host)|on\s+(here|my\s+desktop)|locally|git\s+(status|log|branch|repo(sitory)?)|repo(sitory)?|branch(es)?|commits?)\b`)

	// An instruction to do something, as opposed to a question about the
	// world. Only consulted after the chat patterns, so "can you tell me a
	// joke" never lands here.
	actionLeadRe = regexp.MustCompile(`(?i)^\s*(please\s+)?(can\s+you\s+|could\s+you\s+|would\s+you\s+|i\s+(want|need)\s+you\s+to\s+|go\s+(ahead\s+and\s+)?)?(create|make|delete|remove|move|copy|rename|install|uninstall|update|upgrade|list|show|find|count|check|open|launch|start|stop|kill|restart|set\s*up|set|change|configure|compress|extract|unzip|untar|zip|convert|clean(\s+up)?|tidy(\s+up)?|organi[sz]e|back\s*up|archive|mount|free\s+up|monitor|inspect|scan|measure|summari[sz]e|print|display|verify|test|fix|debug|diagnose|enable|disable|schedule|sort|merge|split|resize|compare)\b`)

	// Knowledge-seeking that the operator's shell is the wrong tool for.
	// A compound request that needs research goes to the planner, whose
	// web steps reach the real web skill instead of a curl command.
	researchRe = regexp.MustCompile(`(?i)\b(find\s+out|research|look\s+(up|into)|learn\s+about|newest|latest|current\s+(price|version\s+of\s+(?:go|python|node|rust|java))|news|search)\b`)
)

var (
	// Something on the desktop's screen - a window, a page already open in
	// the browser, a form, an office document - as opposed to the machine's
	// files and processes or the web at large.
	screenSurfaceRe = regexp.MustCompile(`(?i)\b(browser|chromium|web ?page|(the|this|that) page|admin panel|dashboard|libre ?office|writer|calc|spreadsheet|mousepad|text editor|file manager|thunar|on (the )?screen|gui|window|(the|this|that) form|dialog|button|dropdown|drop-down|check ?box|(the|a|new) tab)\b`)
	// Doing something there. Both halves are required: "browser test message
	// one" names a surface and asks for nothing, and stays conversation.
	screenActionRe = regexp.MustCompile(`(?i)\b(click|double[- ]click|right[- ]click|tick|untick|fill\s+(in|out)|type|press|select|choose|pick|enter|add|edit|change|rename|delete|remove|submit|sign\s+in|log\s*in|save|set|make|turn\s+(on|off)|enable|disable|pay|send|publish|create|open|close|replace|put|scroll)\b`)
)

// wantsScreen reports whether a request is work in a graphical app. The web
// skill would otherwise claim anything with a URL in it and fetch the page
// itself - fine for reading the web, useless for signing in to an admin
// panel and pressing its buttons, which only the operator's `gui` command
// can do.
func wantsScreen(prompt string) bool {
	return screenSurfaceRe.MatchString(prompt) && matchesWord(screenActionRe, prompt)
}

// matchesWord reports whether re matches somewhere that is a word of its
// own, not part of a hyphenated one: "content-type" is a header, not a
// request to type, and "multi-select" asks nothing to be selected. Measured
// live: "run curl -sI https://example.com in the terminal and tell me the
// content-type header" went to the browser extension for its "type".
func matchesWord(re *regexp.Regexp, prompt string) bool {
	for _, loc := range re.FindAllStringIndex(prompt, -1) {
		if loc[0] > 0 && prompt[loc[0]-1] == '-' {
			continue
		}
		if loc[1] < len(prompt) && prompt[loc[1]] == '-' {
			continue
		}
		return true
	}
	return false
}

// shellRe is a request that names where it wants to be done, and it is not a
// web page: "in the terminal", "from the shell", "on the command line".
var shellRe = regexp.MustCompile(`(?i)\b(terminal|shell|command[\s-]?line|bash|console)\b`)

var (
	// The explicit ask, in the user's own words: "browse the web", "browse
	// to X". Deliberately claimed here rather than left to webRe's
	// "browse ... web/internet/online/for" pattern (which would otherwise
	// send it to the read-only web-fetch skill) - saying "browse" at all is
	// asking for a real, interactive session in the browser, not a fetch.
	browsePhraseRe = regexp.MustCompile(`(?i)\bbrowse\s+(the\s+)?(web|internet)\b|\bbrowse\s+to\b`)
	// A concrete place on the web: a URL, a www./bare domain, or a "go to/
	// visit/open ... website/page/site" phrase.
	webDestinationRe = regexp.MustCompile(`(?i)https?://\S+|\bwww\.\S+|\b[a-z0-9][a-z0-9-]*\.(com|org|net|io|co|dev|app|gov|edu|ai)\b|\b(go\s+to|visit|navigate\s+to|open)\b[\s\S]{0,40}?\b(website|web\s?page|webpage|site)\b`)
	// Doing something there, not just reading it - the same distinction
	// wantsScreen draws with screenActionRe, but for a page named by
	// address rather than by the word "browser"/"tab".
	webInteractionRe = regexp.MustCompile(`(?i)\b(search|type|fill|click|log\s?in|sign\s?in|submit|select|choose|buy|add\b[\s\S]{0,20}?\bto\s+(the\s+)?cart|\bcart\b|checkout|scroll|do\s+(that|this)|then\b)`)
)

// wantsBrowser reports whether a request is an interactive job in a real,
// live web page - opening a site and then doing something on it - as
// opposed to reading the web at large (the web skill) or working some other
// app on the screen (the operator, via wantsScreen). Checked before both:
// a named destination plus an action on it is squarely what the
// browser-agent extension exists for, and it drives the actual DOM through
// selector-grounded actions rather than the operator's coordinates/xdotool.
func wantsBrowser(prompt string) bool {
	if browsePhraseRe.MatchString(prompt) {
		return true
	}
	if shellRe.MatchString(prompt) {
		return false
	}
	return webDestinationRe.MatchString(prompt) && matchesWord(webInteractionRe, prompt)
}

// greetingLeadRe strips the greeting off the front of a turn, so what
// follows it can be judged on its own.
var greetingLeadRe = regexp.MustCompile(`(?i)^\s*(hi+|hello+|hey+|yo|hiya|howdy|greetings|good\s+(morning|afternoon|evening|night)|thanks?|thank\s+you|thx|ok(ay)?)\b[\s,.!-]*(there\b[\s,.!-]*)?`)

func isConversational(prompt string) bool {
	if selfRe.MatchString(prompt) || statementRe.MatchString(prompt) {
		return true
	}
	if !greetingRe.MatchString(prompt) {
		return false
	}
	// "hello" is chat; "hello, please install jq" is an instruction with
	// manners. Only the part after the greeting decides.
	rest := greetingLeadRe.ReplaceAllString(prompt, "")
	return !touchesMachine(rest) && !actionLeadRe.MatchString(rest)
}

func touchesMachine(prompt string) bool {
	return machinePathRe.MatchString(prompt) || machineNounRe.MatchString(prompt)
}

// Triage decides the shape of a whole request without calling a model.
//
// The order is what makes it safe. A coding request is handed to the build
// harness whole before anything can split it. A compound request is checked
// before a single classifier may claim it (the live bug that forced this:
// "find out the newest Go version and save a note" matched file-write and
// never searched anything). Conversation is recognised before machine
// signals, so "what's your name?" is answered rather than looked up. And
// whatever is left - no classifier, not conversation - goes to the operator
// only when it names something on this machine or reads as an instruction;
// otherwise it is a question for chat.
func Triage(prompt string) Route {
	p := strings.TrimSpace(prompt)
	matched := Classify(p)

	if matched == "coding" {
		return Route{Kind: RouteSkill, TaskType: matched, Reason: "a request to build software - the coding harness takes it whole"}
	}

	// Before wantsScreen and the compound check, for the same reason
	// wantsScreen already goes first: "Open google.com. Search for this.
	// Do that." is one goal on one page, worked through by the extension's
	// own loop - not a plan written before the first page has even loaded,
	// and not the operator's generic, coordinate-driven screen loop when a
	// page-aware, selector-grounded tool is exactly what this is.
	if wantsBrowser(p) {
		return Route{Kind: RouteBrowser, TaskType: "browser", Reason: "an interactive task in a real web page - handing it to the browser extension"}
	}

	// Before the compound check: "fill in the name, then press Save" is one
	// goal on one screen, worked through by one loop that sees each result -
	// not a plan written before anything on the screen has been looked at.
	if wantsScreen(p) && !researchRe.MatchString(p) {
		return Route{Kind: RouteOperate, TaskType: matched, Reason: "work in an app on the screen - the operator drives it with the gui command"}
	}

	if looksCompound(p) {
		// Several different jobs that all happen on this machine are one
		// goal for the operator, which runs them in one loop with every
		// result in front of it. Anything that needs research, a download,
		// or a file-write/web skill of its own is planned instead.
		if (matched == "general" || matched == "terminal") && !researchRe.MatchString(p) && touchesMachine(p) {
			return Route{Kind: RouteOperate, TaskType: matched, Reason: "several steps, all on this machine - the operator works through them in one loop"}
		}
		reason := "asks for more than one thing"
		if matched != "general" {
			reason = fmt.Sprintf("recognised as %q, but it asks for more than one thing", matched)
		}
		return Route{Kind: RoutePlan, TaskType: matched, Reason: reason}
	}

	if matched != "general" {
		return Route{Kind: RouteSkill, TaskType: matched, Reason: fmt.Sprintf("recognised as a %q task", matched)}
	}

	if isConversational(p) {
		return Route{Kind: RouteChat, TaskType: matched, Reason: "conversation - answering directly"}
	}

	if touchesMachine(p) || actionLeadRe.MatchString(greetingLeadRe.ReplaceAllString(p, "")) {
		return Route{Kind: RouteOperate, TaskType: matched, Reason: "something to do or check on this machine - handing it to the operator"}
	}

	return Route{Kind: RouteChat, TaskType: matched, Reason: "a question - answering directly"}
}
