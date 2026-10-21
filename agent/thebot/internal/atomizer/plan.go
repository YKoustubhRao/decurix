package atomizer

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"thebot/internal/llm"
)

// Step is one node of a plan.
type Step struct {
	// ID is the step's 1-based position in the plan. Needs refer to it.
	ID int
	// Text is the step phrased so its skill can act on it directly - a
	// [shell] step's command becomes "run the command: ...", a [web]
	// query becomes "search the web for ...".
	Text string
	// Type is the task type the step runs as, the same vocabulary Classify
	// produces, plus "operator" for goals the operator skill works out.
	Type string
	// Needs lists the IDs of earlier steps whose results this one uses.
	// The scheduler will not start a step before all of them finish, and
	// runs steps whose needs are all met at the same time.
	Needs []int
	// Tool is the tag the planner wrote ("web", "do", ...), empty when the
	// step came from an untagged line.
	Tool string
}

// Plan is what Atomize produces: the steps to run, and whether this was
// judged real work at all.
type Plan struct {
	Steps  []Step
	IsTask bool
	Route  Route
	Reason string
	// Question is set only by Replan, when the model concluded the rest of
	// the request cannot be done without the human.
	Question string
}

// Texts returns the step texts in plan order.
func (p Plan) Texts() []string {
	out := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = s.Text
	}
	return out
}

// toolList is the capability menu every planning prompt shows. Written as
// tags rather than phrasing conventions: the old planner had to phrase each
// step so a regex would later route it ("search the web for X", "run the
// command: X"), and a small model that paraphrased ("look online for X")
// silently sent the step to plain chat. A tag is one token the model either
// writes or doesn't, and an untagged line still falls back to Classify.
const toolList = `[do] a goal on this computer - the agent chooses and runs the shell commands itself
[shell] one exact shell command
[web] a web search query - the agent searches and reads the top pages
[read] one web page URL to read
[browser] an interactive job in a real web page - open a site, search, click, fill in a form
[download] what to download, and from which URL (or "the search results")
[write] what to write, and the file path to write it to
[code] a program to design, build and test
[docs] a search of documents that are already indexed
[answer] something to work out or explain without any tool`

// The "never write down a fact" rule is there because of a measured plan:
// asked to find the newest Go version and save a note about it, the 3B
// planner wrote step 3 as: write "The newest Go version is v1.20.5" to
// go-version.md - its own guess, baked into the step before the search in
// step 1 had run, and exactly what file-write would then have saved.
const planRules = `Rules:
- Use the fewest steps that do the job, at most 10. Writing a file already saves it.
- Never write facts into a step that earlier steps are meant to find out. Say what to write ("a note about the newest Go version"), not what it will say.
- Never plan GUI actions on the desktop itself (opening an editor, clicking a window). A step that needs an actual website opened and worked (search it, click something on it, fill in a form) is [browser], not [do].
- When a step uses the result of earlier steps, end it with (after N) or (after N, M).
- Do not add a last step that reports back - that happens automatically.
- Reply with the numbered steps only.`

// planPrompt is sent only for a request Triage judged compound, so it never
// asks "is this a task?" - that was already established without a model.
// Asking it anyway is what let a model answer "TASK: no" to a real download
// request, consistently, at any temperature.
const planPrompt = `Plan the request below as numbered steps. Every step starts with exactly one of these tools:
` + toolList + `

` + planRules + `

Example
Request: find out the newest Go version and save a short note about it to go-version.md
1. [web] newest Go version release
2. [write] a short note about the newest Go version to go-version.md (after 1)

Request: %s`

// decomposePrompt turns failure-reasoning's plain description of a
// multi-step fix into steps. It always plans - DECOMPOSE already means
// "this needs more than one step", so there is nothing to triage.
const decomposePrompt = `A step failed, and fixing it takes more than one step. Plan the fix as numbered steps. Each step runs in a fresh process, so nothing like "activate" or "cd" carries over from one step to the next. Every step starts with exactly one of these tools:
` + toolList + `

` + planRules + `

What needs to happen: %s`

// replanPrompt is the scheduler's second chance at a plan whose step
// failed even after its own recovery.
const replanPrompt = `You are partway through this request: %s

What has happened so far:
%s

Plan the remaining steps to finish the request another way, working around what failed. Do not repeat steps that already succeeded - their results are kept. Every step starts with exactly one of these tools:
` + toolList + `

` + planRules + `

If the request genuinely cannot be finished without the human, reply with only: ASK: <what you need from them>`

var (
	// "3. [web] query (after 1, 2)" - the tag is optional so a plain
	// numbered list (the old format, and what a model falls back to)
	// still parses.
	stepLineRe = regexp.MustCompile(`^\s*(?:step\s*)?(\d+)\s*[.):-]\s*(.+)$`)
	tagRe      = regexp.MustCompile(`^(?:\*\*)?\[\s*([A-Za-z-]+)\s*\](?:\*\*)?\s*:?\s*(.*)$`)
	colonTagRe = regexp.MustCompile(`^(?:\*\*)?([A-Za-z]+)(?:\*\*)?\s*:\s+(.*)$`)
	needsRe    = regexp.MustCompile(`(?i)\s*\(\s*(?:after|needs?|uses?|using|depends\s+on|from|based\s+on)\s*:?\s*(?:steps?\s*)?([0-9][0-9,\s&and]*)\)\s*\.?\s*$`)
	// "(independent)" or "(after none)" states that a step needs nothing,
	// which is what lets it start alongside the steps before it.
	independentRe = regexp.MustCompile(`(?i)\s*\(\s*(?:independent|parallel|after\s+none|needs\s+nothing|no\s+dependencies)\s*\)\s*\.?\s*$`)
	needsNumRe    = regexp.MustCompile(`\d+`)
	askRe         = regexp.MustCompile(`(?im)^\s*ASK:\s*(.+)$`)
	runLeadRe     = regexp.MustCompile(`(?i)^\s*(run|execute)\b`)
	writeLeadRe   = regexp.MustCompile(`(?i)^\s*(write|save|create|generate|put)\b`)
	// A [shell] step that is really an English instruction ("check how much
	// disk space is free") is a goal, not a command - running it verbatim
	// would execute a sentence.
	proseLeadRe = regexp.MustCompile(`(?i)^\s*(check|find|figure|determine|verify|ensure|make\s+sure|see|look|get|identify|confirm|count|list\s+all|work\s+out|inspect)\b`)
	// A trailing step that only restates "tell the user" adds a model call
	// to say what the scheduler's synthesis says anyway.
	reportStepRe = regexp.MustCompile(`(?i)^\s*(report|tell|inform|summari[sz]e|present|answer|respond|reply|give|provide|share|explain)\b[\s\S]{0,60}\b(user|human|results?|findings|answer|them|back)\b`)
)

// toolTypes maps a planner's tag to the task type its step runs as.
var toolTypes = map[string]string{
	"do": "operator", "operate": "operator", "task": "operator", "computer": "operator",
	"shell": "terminal", "run": "terminal", "command": "terminal", "cmd": "terminal", "terminal": "terminal", "bash": "terminal",
	"web": "web", "search": "web", "read": "web", "url": "web", "fetch": "web",
	"browser": "browser", "browse": "browser", "site": "browser",
	"download": "download",
	"write":    "file-write", "file": "file-write", "save": "file-write",
	"code": "coding", "build": "coding", "coding": "coding",
	"docs": "rag-query", "rag": "rag-query", "documents": "rag-query",
	"answer": "general", "reply": "general", "respond": "general", "chat": "general", "think": "general",
}

// researchTypes are steps that only read. Consecutive ones with no stated
// dependencies run side by side.
var researchTypes = map[string]bool{"web": true, "rag-query": true}

// parsePlan turns a model's numbered, tagged list into steps: each routed
// to a task type, phrased for its skill, no-ops dropped, capped, and wired
// into a dependency graph.
func parsePlan(text string, onLog func(string)) []Step {
	var parsed []planLine

	for _, line := range strings.Split(text, "\n") {
		m := stepLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		num, _ := strconv.Atoi(m[1])
		body := strings.TrimSpace(m[2])

		var needs []int
		given := false
		if nm := needsRe.FindStringSubmatch(body); nm != nil {
			for _, d := range needsNumRe.FindAllString(nm[1], -1) {
				if n, err := strconv.Atoi(d); err == nil && n < num {
					needs = append(needs, n)
				}
			}
			given = true
			body = strings.TrimSpace(body[:len(body)-len(nm[0])])
		} else if im := independentRe.FindString(body); im != "" {
			given = true
			body = strings.TrimSpace(body[:len(body)-len(im)])
		}

		tool := ""
		if tm := tagRe.FindStringSubmatch(body); tm != nil {
			tool, body = strings.ToLower(tm[1]), strings.TrimSpace(tm[2])
		} else if cm := colonTagRe.FindStringSubmatch(body); cm != nil {
			if _, known := toolTypes[strings.ToLower(cm[1])]; known {
				tool, body = strings.ToLower(cm[1]), strings.TrimSpace(cm[2])
			}
		}
		body = strings.TrimSpace(strings.Trim(body, "*"))
		if body == "" {
			continue
		}

		parsed = append(parsed, planLine{num: num, step: shapeStep(tool, body), needs: needs, given: given})
	}

	// No-ops first, so a dropped step never takes up a slot under the cap.
	kept := parsed[:0:0]
	dropped := 0
	for _, r := range parsed {
		if noOpStepRe.MatchString(r.step.Text) {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	for len(kept) > 1 {
		last := kept[len(kept)-1].step
		if last.Type != "general" || !(last.Tool == "answer" || last.Tool == "reply" || last.Tool == "respond" || reportStepRe.MatchString(last.Text)) {
			break
		}
		kept = kept[:len(kept)-1]
		dropped++
	}
	if dropped > 0 && onLog != nil {
		onLog(fmt.Sprintf("atomizer: dropped %d step(s) that no skill needs to act on", dropped))
	}
	if len(kept) > maxSteps {
		if onLog != nil {
			onLog(fmt.Sprintf("atomizer: plan had %d real step(s), capping at %d - the request may need to be split up", len(kept), maxSteps))
		}
		kept = kept[:maxSteps]
	}

	// Renumber densely, remapping stated dependencies onto the new IDs and
	// forgetting any that pointed at a dropped step.
	idFor := map[int]int{}
	for i, r := range kept {
		idFor[r.num] = i + 1
	}
	steps := make([]Step, len(kept))
	given := make([]bool, len(kept))
	for i, r := range kept {
		s := r.step
		s.ID = i + 1
		given[i] = r.given
		if r.given {
			seen := map[int]bool{}
			for _, n := range r.needs {
				if id, ok := idFor[n]; ok && id < s.ID && !seen[id] {
					seen[id] = true
					s.Needs = append(s.Needs, id)
				}
			}
			sort.Ints(s.Needs)
		}
		steps[i] = s
	}
	wireDefaultNeeds(steps, given)
	return steps
}

// planLine is one numbered line as the model wrote it, before renumbering.
type planLine struct {
	num   int
	step  Step
	needs []int
	given bool
}

// wireDefaultNeeds fills in dependencies for steps the planner did not
// annotate. The default is strictly sequential - a plan is written in the
// order it should happen, and a small model that forgets "(after 1)" must
// not get its write step run before the search it depends on. The one
// exception is a run of read-only research steps, which cannot affect each
// other and so share their predecessor instead of queueing behind one
// another; the step after such a run waits for all of it.
func wireDefaultNeeds(steps []Step, given []bool) {
	var group []int // IDs of the current run of parallel research steps
	for i := range steps {
		if given[i] {
			group = nil
			if researchTypes[steps[i].Type] {
				group = []int{steps[i].ID}
			}
			continue
		}
		if i == 0 {
			if researchTypes[steps[i].Type] {
				group = []int{steps[i].ID}
			}
			continue
		}
		prev := steps[i-1]
		switch {
		case researchTypes[steps[i].Type] && researchTypes[prev.Type] && len(group) > 0:
			steps[i].Needs = append([]int(nil), prev.Needs...)
			group = append(group, steps[i].ID)
		case len(group) > 0 && researchTypes[prev.Type]:
			steps[i].Needs = append([]int(nil), group...)
			group = nil
		default:
			steps[i].Needs = []int{prev.ID}
			group = nil
			if researchTypes[steps[i].Type] {
				group = []int{steps[i].ID}
			}
		}
	}
}

// shapeStep routes one planned line to a task type and phrases it the way
// that type's skill reads its input.
func shapeStep(tool, body string) Step {
	typ, known := toolTypes[tool]
	if !known {
		typ = Classify(body)
		tool = ""
	}
	typ = withoutMissingBrowser(typ)

	// A command is not a program to build. The 3B planner tagged
	// `grep -oP '(?<=go-)\d+\.\d+\.\d+' index.html | sort -nr | head -n 1` as
	// [code], which would have started a whole design-scaffold-test build
	// for one pipeline.
	if typ == "coding" && looksLikeCommandLine(body) {
		typ = "terminal"
	}

	// Nor is a file with no code in it. The same planner tagged "Create a
	// new file named koustubh.txt" as [code], and the coding harness spent
	// its whole timeout designing an architecture for a text file. A [code]
	// step that names no software goes where its words send it.
	if typ == "coding" && !describesSoftware(body) {
		typ = Classify(body)
		if typ == "general" {
			// A tagged step is something to do, never a question for chat.
			typ = "operator"
		}
	}

	text := body
	switch typ {
	case "terminal":
		cmd := strings.TrimSpace(runLeadRe.ReplaceAllString(body, ""))
		cmd = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(cmd, "the command")), ":"))
		if !strings.Contains(body, "`") && proseLeadRe.MatchString(cmd) && !shellSyntaxHintRe.MatchString(cmd) {
			// An English goal wearing a [shell] tag.
			typ = "operator"
			break
		}
		if !runLeadRe.MatchString(body) {
			text = "run the command: " + body
		}
	case "web":
		if tool == "read" || urlOnlyRe.MatchString(body) || siteRe.MatchString(body) || Classify(body) == "web" {
			break
		}
		text = "search the web for " + body
	case "download":
		if !downloadRe.MatchString(body) && !strings.HasPrefix(strings.ToLower(body), "download") {
			text = "download " + body
		}
	case "file-write":
		if !writeLeadRe.MatchString(body) {
			text = "write " + body
		}
	case "rag-query":
		if Classify(body) != "rag-query" {
			text = "search the indexed documents for " + body
		}
	}
	return Step{Text: text, Type: typ, Tool: tool}
}

// commandLineRe is a line that starts with a common command and carries
// shell syntax - a pipe, a redirect, a flag.
var commandLineRe = regexp.MustCompile(`^\s*(sudo\s+)?(grep|egrep|rg|sed|awk|cat|head|tail|ls|find|curl|wget|sort|uniq|wc|cut|tr|echo|printf|cp|mv|rm|mkdir|chmod|chown|tar|unzip|zip|git|pip3?|npm|npx|go|cargo|python3?|node|bash|sh|df|du|ps|kill|systemctl|docker|apt(-get)?|jq|xargs|touch|ln|diff)\s`)

func looksLikeCommandLine(s string) bool {
	return commandLineRe.MatchString(s) && shellSyntaxHintRe.MatchString(s)
}

var shellSyntaxHintRe = regexp.MustCompile("&&|\\|\\||[|;<>]|\\$\\(|\\s-{1,2}[a-zA-Z]|^\\S+$|`")

// quotedContentRe finds a literal a step says to write: "..." or '...' or
// curly quotes, long enough to be content rather than a filename.
var quotedContentRe = regexp.MustCompile(`"([^"]{8,})"|“([^”]{8,})”|'([^']{8,})'`)

// scrubInventedContent removes content the planner wrote into a step that
// depends on research, when that content is not the human's own words.
//
// The rule in planRules asks for this and a 3B planner ignores it: after the
// rule was added it still planned `write "The newest Go version is v1.20.5"
// to go-version.md` behind a search that went on to find 1.27 - and
// file-write, told both the step and the findings, saved the step's guess.
// A quoted literal in a write that follows research is only trustworthy if
// the human typed it; anything else becomes a pointer to the findings.
func scrubInventedContent(steps []Step, request string, onLog func(string)) {
	lowered := strings.ToLower(request)
	researched := false
	for i := range steps {
		if researchTypes[steps[i].Type] || steps[i].Type == "download" {
			researched = true
			continue
		}
		if !researched || (steps[i].Type != "file-write" && steps[i].Type != "general") {
			continue
		}
		scrubbed := quotedContentRe.ReplaceAllStringFunc(steps[i].Text, func(q string) string {
			inner := strings.Trim(q, `"'“”`)
			if strings.Contains(lowered, strings.ToLower(inner)) {
				return q
			}
			return "the findings from the earlier steps"
		})
		if scrubbed != steps[i].Text {
			if onLog != nil {
				onLog(fmt.Sprintf("atomizer: step %d had content written in before anything was looked up - it will use what the earlier steps find instead", steps[i].ID))
			}
			steps[i].Text = scrubbed
		}
	}
}

// ensureCoverage checks that a plan still does the job the whole request was
// classified as, and adds the step when it does not.
//
// Measured on the 3B planner, same request twice ("find out the newest Go
// version and save a short note about it to go-version.md"): once its last
// step was a proper write; the next time it was `grep "go" -A 5 index.html |
// tail -n 1 > go-version.md` against a file that did not exist, no step
// wrote the note, and the job "answered" without it. A request the
// classifiers already know is a file-write is not done until something
// writes that file, so a plan with no write step naming it gets one - the
// whole request, run after everything else, where file-write sees every
// finding and takes its filename from the human's own words.
func ensureCoverage(steps []Step, request string, route Route, onLog func(string)) []Step {
	if route.TaskType != "file-write" || len(steps) == 0 {
		return steps
	}
	target := strings.ToLower(filenameInRequestRe.FindString(request))
	for _, st := range steps {
		if st.Type != "file-write" {
			continue
		}
		if target == "" || strings.Contains(strings.ToLower(st.Text), target) {
			return steps
		}
	}

	// Anything else that tried to produce that file (a shell redirect, an
	// operator goal) is replaced by the real write.
	kept := steps[:0:0]
	for _, st := range steps {
		if target != "" && !researchTypes[st.Type] && strings.Contains(strings.ToLower(st.Text), target) {
			continue
		}
		kept = append(kept, st)
	}
	needs := make([]int, 0, len(kept))
	for i := range kept {
		needs = append(needs, kept[i].ID)
	}
	kept = append(kept, Step{ID: len(steps) + 1, Text: request, Type: "file-write", Needs: needs})
	if onLog != nil {
		onLog("atomizer: the plan never wrote the file the request asks for - adding that step at the end")
	}
	return renumber(kept)
}

var filenameInRequestRe = regexp.MustCompile(`\b[\w-]+(?:/[\w-]+)*\.[a-zA-Z0-9]{1,8}\b`)

// renumber makes step IDs dense again after steps were removed, remapping
// dependencies and dropping any that pointed at a removed step.
func renumber(steps []Step) []Step {
	idFor := map[int]int{}
	for i, st := range steps {
		idFor[st.ID] = i + 1
	}
	for i := range steps {
		var needs []int
		for _, n := range steps[i].Needs {
			if id, ok := idFor[n]; ok && id < i+1 {
				needs = append(needs, id)
			}
		}
		steps[i].ID, steps[i].Needs = i+1, needs
	}
	return steps
}

// Planner makes the model calls this package needs. The zero value is not
// usable; Provider and Model must be set.
type Planner struct {
	Provider llm.Provider
	Model    string
	// System is the job's shared system frame (see internal/prompt). When
	// set, planning calls open with it, so they reuse the prompt prefix
	// every other call in the job already paid to evaluate.
	System *llm.Message
	OnLog  func(string)
}

func (p Planner) log(line string) {
	if p.OnLog != nil {
		p.OnLog(line)
	}
}

func (p Planner) messages(user string) []llm.Message {
	if p.System != nil && p.System.Content != "" {
		return []llm.Message{*p.System, {Role: "user", Content: user}}
	}
	return []llm.Message{{Role: "user", Content: user}}
}

// planOptions are shared by every planning call: low temperature, because
// a plan is a structured artifact, not prose. The same planning prompt sent
// twice at Ollama's default temperature (~0.8) came back with different
// answers to the same question.
var planOptions = llm.Options{TimeoutMs: 120000, Temperature: 0.15}

func single(prompt, taskType string, isTask bool, route Route) Plan {
	return Plan{
		Steps:  []Step{{ID: 1, Text: prompt, Type: taskType}},
		IsTask: isTask,
		Route:  route,
		Reason: route.Reason,
	}
}

// Atomize decides the shape of a request and, when it is compound, plans
// it. Only RoutePlan costs a model call.
func (p Planner) Atomize(ctx context.Context, prompt string) Plan {
	route := Triage(prompt)
	p.log("atomizer: " + route.Reason)

	switch route.Kind {
	case RouteChat:
		return single(prompt, "general", false, route)
	case RouteSkill:
		return single(prompt, route.TaskType, true, route)
	case RouteOperate:
		return single(prompt, "operator", true, route)
	case RouteBrowser:
		return single(prompt, "browser", true, route)
	}

	// Planning failed or produced nothing: run the whole request as one
	// step rather than give up. Triage already established it is real work
	// - a bad planning response cannot undo that.
	fallback := func(why string) Plan {
		typ := route.TaskType
		if typ == "general" {
			typ = "operator"
		}
		p.log(fmt.Sprintf("atomizer: %s - running the whole request as one %s step", why, typ))
		plan := single(prompt, typ, true, route)
		plan.Reason = why
		return plan
	}

	if p.Provider == nil {
		return fallback("no model available to plan with")
	}

	p.log("atomizer: planning the steps...")
	resp, err := p.Provider.Chat(ctx, p.Model, p.messages(fmt.Sprintf(planPrompt, prompt)), planOptions)
	if err != nil {
		return fallback("the planning call failed (" + err.Error() + ")")
	}
	steps := parsePlan(resp.Text, p.OnLog)
	if len(steps) == 0 {
		return fallback("the plan came back with no usable steps")
	}
	scrubInventedContent(steps, prompt, p.OnLog)
	steps = ensureCoverage(steps, prompt, route, p.OnLog)
	if len(steps) == 1 && route.TaskType != "general" && steps[0].Type == "general" {
		// A one-line "plan" that turned a classified request into chat
		// would downgrade real work; the classifier already knew better.
		return fallback("the plan collapsed to a single chat step")
	}

	p.logPlan("planned", steps)
	return Plan{Steps: steps, IsTask: true, Route: route, Reason: route.Reason}
}

// Decompose plans a recovery the failure reasoner described in words. It
// never triages: the reasoner already decided the fix takes several steps.
func (p Planner) Decompose(ctx context.Context, description string) Plan {
	route := Route{Kind: RoutePlan, TaskType: Classify(description), Reason: "decomposing a recovery"}
	if p.Provider == nil {
		return Plan{Route: route, Reason: "no model available to decompose with"}
	}
	resp, err := p.Provider.Chat(ctx, p.Model, p.messages(fmt.Sprintf(decomposePrompt, description)), planOptions)
	if err != nil {
		p.log("atomizer: decomposition call failed (" + err.Error() + ")")
		return Plan{Route: route, Reason: "decomposition call failed"}
	}
	steps := parsePlan(resp.Text, p.OnLog)
	if len(steps) > 0 {
		p.logPlan("recovery plan", steps)
	}
	return Plan{Steps: steps, IsTask: len(steps) > 0, Route: route, Reason: route.Reason}
}

// Replan asks for the rest of a plan after a step failed for good. history
// is a plain-text account of what ran and what came of it. The returned
// plan either has steps, or a Question for the human, or neither (the call
// failed).
func (p Planner) Replan(ctx context.Context, goal, history string) Plan {
	route := Route{Kind: RoutePlan, TaskType: Classify(goal), Reason: "replanning after a failure"}
	if p.Provider == nil {
		return Plan{Route: route}
	}
	resp, err := p.Provider.Chat(ctx, p.Model, p.messages(fmt.Sprintf(replanPrompt, goal, history)), planOptions)
	if err != nil {
		p.log("atomizer: replanning call failed (" + err.Error() + ")")
		return Plan{Route: route, Reason: "replanning call failed"}
	}
	if m := askRe.FindStringSubmatch(resp.Text); m != nil && len(parsePlan(resp.Text, nil)) == 0 {
		return Plan{Route: route, Question: strings.TrimSpace(m[1])}
	}
	steps := parsePlan(resp.Text, p.OnLog)
	scrubInventedContent(steps, goal, p.OnLog)
	if len(steps) > 0 {
		p.logPlan("new plan for the rest", steps)
	}
	return Plan{Steps: steps, IsTask: len(steps) > 0, Route: route, Reason: route.Reason}
}

func (p Planner) logPlan(label string, steps []Step) {
	p.log(fmt.Sprintf("atomizer: %s - %d step(s):", label, len(steps)))
	for _, s := range steps {
		line := fmt.Sprintf("atomizer:   %d. [%s] %s", s.ID, s.Type, s.Text)
		if len(s.Needs) > 0 {
			parts := make([]string, len(s.Needs))
			for i, n := range s.Needs {
				parts[i] = strconv.Itoa(n)
			}
			line += " (after " + strings.Join(parts, ", ") + ")"
		}
		p.log(line)
	}
}

// maxSteps caps how large a single plan can get. Raised from 5 to 10
// after a live failure: "write C++ implementations of X, Y, and Z, each
// in its own file, searching the web first for any you don't know well"
// genuinely needs up to 2 steps per item (search, then write) - 3 items
// is already 6, past the old cap. The cap silently truncated to 5,
// dropping the LAST write step with no signal anywhere that anything was
// cut.
const maxSteps = 10
