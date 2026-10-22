package skills

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"thebot/internal/llm"
)

// ============================================================
// When the model is not sure: settle it from a source, the web or the person
//
// Found live (GST suite T9, 2026-10-22): a strong model's judgement calls quoted words that were really in the
// documents - and did not establish the call ("0 cancelled | backed by: the invoice list 451-458"). Code can tell a
// quote is real, not that it proves the point. The open-source uncertainty tools (LM-Polygraph, UQLM, SelfCheckGPT)
// mostly read token probabilities, which a hosted reasoning model does not give; their black-box way - a second,
// independent look at whether the evidence supports the claim - is what this does:
//
//   - each call whose quote is real is put, with only its quote and the request, to an independent model call that
//     answers yes (the words establish it), no, or unsure. "no" and "unsure" are calls the model is not sure of;
//   - those, and the calls nothing backs, go back to the operator once: settle each - re-read the source and quote the
//     words that establish it, or look it up on the web (lookup, with its link), or ask the person for the rule or
//     its documentation;
//   - what is still not settled goes to the person as one question. A call backed only by a web page the goal looked
//     up is asked as "is this a source you trust for this?", with its link - a page is never the work's source until
//     the person says so; their yes makes it one for this goal, and their answer is kept as a lesson (lessons.py).
//   - a link the person gives (the request, a reply) is theirs: what the work reads there is a source at once.
//
// Nothing here knows a domain: the model says what its calls are and what backs them; code checks the quote exists,
// a separate model call checks it establishes the call, and the person settles the rest.
// ============================================================

const (
	maxLookups       = 12
	maxLookupChars   = 30_000
	maxPersonLinks   = 50
	maxSupportItems  = 20
	supportTimeoutMs = 120_000
)

// webLookup is one web page or search the goal read: the command, the links its output names, and its text.
type webLookup struct {
	Command string
	Links   []string
	Text    string
}

// linkRe is a web link.
var linkRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)

// noteLookup keeps what a web lookup printed, apart from what the goal has seen as sources.
func (st *operatorState) noteLookup(command, output string) {
	links := linkRe.FindAllString(command+"\n"+output, 8)
	st.Lookups = append(st.Lookups, webLookup{Command: truncateText(command, 400), Links: links, Text: truncateText(output, maxLookupChars)})
	if len(st.Lookups) > maxLookups {
		st.Lookups = st.Lookups[len(st.Lookups)-maxLookups:]
	}
}

// notePersonLinks keeps the links the person gave: what the work reads there is theirs.
func (st *operatorState) notePersonLinks(text string) {
	for _, l := range linkRe.FindAllString(text, -1) {
		l = strings.TrimRight(l, ".,;:")
		if !contains(st.PersonLinks, l) && len(st.PersonLinks) < maxPersonLinks {
			st.PersonLinks = append(st.PersonLinks, l)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// fetchesPersonLink reports whether a command reads a page the person linked.
func (st *operatorState) fetchesPersonLink(command string) bool {
	for _, l := range st.PersonLinks {
		if strings.Contains(command, l) {
			return true
		}
	}
	return false
}

// settleAskedSources takes the person's reply to "is this a source you trust?": anything but a no makes the pages
// sources for the rest of the goal.
func (st *operatorState) settleAskedSources(reply string) {
	if len(st.AskedSources) == 0 {
		return
	}
	if !refusalReplyRe.MatchString(reply) {
		for _, i := range st.AskedSources {
			if i >= 0 && i < len(st.Lookups) {
				st.noteSeen(st.Lookups[i].Text)
			}
		}
	}
	st.AskedSources = nil
}

// lookupBacking is the web page an item's quote comes from, as (index, link), or (-1, "") when none holds it.
func (st *operatorState) lookupBacking(item string) (int, string) {
	backing := item
	if loc := backedByRe.FindStringIndex(item); loc != nil {
		backing = item[loc[1]:]
	}
	for _, m := range judgementQuoteRe.FindAllStringSubmatch(backing, -1) {
		for i := len(st.Lookups) - 1; i >= 0; i-- {
			if quoteSeen(m[1], " "+plainWords(st.Lookups[i].Text)+" ") {
				link := ""
				if len(st.Lookups[i].Links) > 0 {
					link = st.Lookups[i].Links[0]
				}
				return i, link
			}
		}
	}
	return -1, ""
}

// openURLRe is a gui open of a web address.
var openURLRe = regexp.MustCompile(`(?i)\bgui\s+open\s+["']?(https?://[^\s"']+)`)

// hostOf is a web address's host (with its port), lower case.
func hostOf(link string) string {
	rest := link[strings.Index(link, "://")+3:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return strings.ToLower(rest)
}

// unknownSiteNudge is what to say instead of opening a web address nothing the goal was given or taught names, for a
// request that is not research: the person's systems are reached where they said. "" to go ahead. Found live (GST suite
// T9, 2026-10-22): asked to file on "the GST portal" (the person's own, taught as a task with its address), the
// operator opened the real government site from memory, and the gate stopped a password going there.
func unknownSiteNudge(st *operatorState, command string) string {
	m := openURLRe.FindStringSubmatch(command)
	if m == nil || researchRe.MatchString(st.Request) || st.fetchesPersonLink(command) {
		return ""
	}
	host := hostOf(m[1])
	if host == "" || strings.Contains(strings.ToLower(st.judgementSources()), host) {
		return ""
	}
	return fmt.Sprintf("Not opened: nothing you were given or taught names %s. The person's own systems are reached at "+
		"the addresses they gave or taught - their request, their lessons, a task they taught (gui skills <words> lists "+
		"each with where it runs; gui skill NAME shows its address). Use that address; if nothing names one, ask the "+
		"person (HELP) where their system is. Never sign in to a site from memory.", host)
}

// supportSystem is the independent look at whether quoted words establish a call.
const supportSystem = `You check a worker's reasons, independently of the worker. Each item is a decision the worker made and the words it quotes as what establishes it. For each, say whether the quoted words, read on their own (with the request for context), establish the decision: "yes" - they do; "no" - they say otherwise; "unsure" - they do not settle it (the decision needs something the words do not say). Judge only the quoted words, not whether the decision sounds plausible or usual. Reply with JSON only: {"items": [{"n": 1, "verdict": "yes", "why": "a few words"}]}`

// unsupportedCalls are the items an independent model call does not find established by their own quoted words, each
// with why. A check that cannot be made decides nothing (none are returned).
func unsupportedCalls(rc RunContext, request string, items []string) []string {
	if len(items) == 0 || rc.Provider == nil {
		return nil
	}
	if len(items) > maxSupportItems {
		items = items[:maxSupportItems]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The request: %s\n\nItems:\n", truncateText(request, 3000))
	for i, item := range items {
		decision, backing := item, ""
		if loc := backedByRe.FindStringIndex(item); loc != nil {
			decision, backing = strings.TrimRight(strings.TrimSpace(item[:loc[0]]), "|"), item[loc[1]:]
		}
		var quotes []string
		for _, m := range judgementQuoteRe.FindAllStringSubmatch(backing, -1) {
			quotes = append(quotes, `"`+m[1]+`"`)
		}
		fmt.Fprintf(&b, "%d. Decision: %s\n   Quoted: %s\n", i+1, strings.TrimSpace(decision), strings.Join(quotes, " and "))
	}
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: supportSystem},
		{Role: "user", Content: b.String()}}, llm.Options{TimeoutMs: supportTimeoutMs, Temperature: 0})
	if err != nil {
		rc.Logf("operator", "could not check what the judgement calls rest on: %v", err)
		return nil
	}
	text := resp.Text
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	var answer struct {
		Items []struct {
			N       int    `json:"n"`
			Verdict string `json:"verdict"`
			Why     string `json:"why"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(text), &answer) != nil {
		return nil
	}
	var out []string
	for _, a := range answer.Items {
		if a.N < 1 || a.N > len(items) || strings.EqualFold(strings.TrimSpace(a.Verdict), "yes") {
			continue
		}
		why := strings.TrimSpace(a.Why)
		if why == "" {
			why = "the words quoted do not settle it"
		}
		out = append(out, items[a.N-1]+" [not settled: "+truncateText(why, 160)+"]")
	}
	return out
}

// weighJudgements decides what follows a list of judgement calls: a nudge back to the operator (the calls it is not
// sure of, once), a question for the person, or neither (go on). action is what the approval is for, "" at a DONE.
func (o *OperatorSkill) weighJudgements(rc RunContext, st *operatorState, items []string, action string) (nudge, question string) {
	if len(items) == 0 {
		return "", ""
	}
	missing := unbackedCalls(items, st.judgementSources())
	isMissing := map[string]bool{}
	for _, m := range missing {
		isMissing[m] = true
	}
	var backed, web, plain []string
	var webIdx []int
	for _, item := range items {
		if !isMissing[item] {
			backed = append(backed, item)
			continue
		}
		if i, link := st.lookupBacking(item); i >= 0 && !notBackedRe.MatchString(item) {
			web = append(web, item+map[bool]string{true: " - from " + link, false: ""}[link != ""])
			webIdx = append(webIdx, i)
			continue
		}
		plain = append(plain, item)
	}
	unsure := append(plain, unsupportedCalls(rc, st.Request, backed)...)

	if len(unsure) > 0 && !st.UnsureSent {
		st.UnsureSent = true
		rc.Logf("operator", "%d judgement call(s) not settled by what they quote - sent back to settle them", len(unsure))
		return settleNudge(unsure), ""
	}
	if (len(unsure) > 0 || len(web) > 0) && !st.JudgementAsked {
		st.JudgementAsked, st.JudgementQuestion = true, true
		st.AskedSources = webIdx
		rc.Logf("operator", "%d call(s) still unsettled, %d resting on the web - asking the person", len(unsure), len(web))
		return "", knowledgeQuestion(unsure, web, action)
	}
	return "", ""
}

// settleNudge sends the calls the model is not sure of back to it, once.
func settleNudge(unsure []string) string {
	var b strings.Builder
	b.WriteString("Not settled yet - these calls are not established by what you quoted:\n")
	for i, c := range unsure {
		fmt.Fprintf(&b, "%d. %s\n", i+1, truncateText(c, 400))
	}
	b.WriteString("Settle each before you go on, without guessing: re-read the source and quote the words that establish " +
		"it; or, if it is a rule or a convention you do not know, look it up (lookup \"...\") and quote the page, with its " +
		"link - the person will be asked whether to trust that source; or ask the person (HELP) for the rule, or for " +
		"documentation or a link to follow. If settling one shows your work is wrong, correct the work first. Then reply " +
		"again with the JUDGEMENTS list.")
	return b.String()
}

// knowledgeQuestion is what the person is asked: web sources to vouch for, and calls nothing settles.
func knowledgeQuestion(unsure, web []string, action string) string {
	var b strings.Builder
	if action != "" {
		fmt.Fprintf(&b, "Before I go on to %s:\n", truncateText(strings.TrimPrefix(action, "on screen it would "), 200))
	} else {
		b.WriteString("Before I report this as done:\n")
	}
	if len(web) > 0 {
		b.WriteString("These rest on what I looked up on the web. Is each a source you trust for this? Reply yes to keep " +
			"them, or send me the documentation or link I should follow instead:\n")
		for i, c := range web {
			fmt.Fprintf(&b, "%d. %s\n", i+1, truncateText(c, 400))
		}
	}
	if len(unsure) > 0 {
		b.WriteString("These are choices I made that nothing you gave me or taught me settles. Tell me how each should be " +
			"treated, or send me documentation or a link for it, or reply \"as you did\" to keep them:\n")
		for i, c := range unsure {
			fmt.Fprintf(&b, "%d. %s\n", i+1, truncateText(c, 400))
		}
	}
	return strings.TrimSpace(b.String())
}
