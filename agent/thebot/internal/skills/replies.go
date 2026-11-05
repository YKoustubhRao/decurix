package skills

import (
	"encoding/json"
	"regexp"
	"strings"

	"thebot/internal/llm"
)

// ============================================================
// A person's reply to a question the harness put to them itself - where a value comes from, whether a web page may be
// trusted, whether the work they looked at is done - read for what it does to what was asked. Found live (2026-10-23):
// asked where "31200" came from, a person answered "31200 for which field? dont type anything u cant point to", and a
// word pattern ("anything that does not start with no") took that for vouching for the value; a "haan its done" was
// no confirmation to the pattern that wanted a bare "done". People answer in their own words: only a reply that plainly
// says yes or no is settled in code; any other is the model's reading. (Approvals are read once by the host,
// skills.Reading.)
// ============================================================

const replySystem = `An AI worker asked a person something (below). Read their reply the way a colleague would - in any language, slang, shorthand or emoji - and say what it does to what they were asked:
- "accepts": they confirm it, vouch for it, or leave it to the worker's own judgement ("yes", "that's right", "haan theek hai", "use your judgement").
- "corrects": they give something else in its place (another value, another page, another answer).
- "points": they say where to find it or how to work it out, without vouching for it ("it's in the sales sheet", "add up those rows").
- "refuses": they say it is wrong, not to use it, or no.
- "asks": they ask the worker something back instead.
And "wants": what they ask the worker to tell them, show them or do before it goes on (a question to answer, a report, a check to make), in their words - empty when they ask nothing of it.
And "refers": a practice, rule or earlier instruction the reply relies on without spelling it out ("as usual", "you know how we treat cancelled ones", "same as last month") - what it is about, in a few words; empty when it spells out everything it relies on.
Reply with JSON only: {"does": "accepts" | "corrects" | "points" | "refuses" | "asks", "wants": "...", "refers": "..."}`

// The words a reply made of nothing else plainly accepts with, or plainly refuses with.
var (
	plainAcceptWords = wordSet("yes yep yeah yup y ok okay k sure fine correct right confirmed confirm done it's it is " +
		"looks good that's use your own judgement judgment please")
	plainRefuseWords = wordSet("no nope don't dont do not never wrong stop that none of it please")
	replyTokenRe     = regexp.MustCompile(`[a-z']+|[^\sa-z'.,!;:-]`)
)

func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}

// plainlyOf reports whether every word of text is one of words.
func plainlyOf(text string, words map[string]bool) bool {
	tokens := replyTokenRe.FindAllString(strings.ToLower(text), -1)
	if len(tokens) == 0 {
		return false
	}
	for _, t := range tokens {
		if !words[t] {
			return false
		}
	}
	return true
}

// replyAccepts reads the person's reply to what they were asked: whether it accepts it (confirms it, vouches for it,
// or leaves it to the worker), and whether that is known. A reply plainly yes or plainly no is settled here; any other
// is the model's reading on a hosted brain. Not known (a local brain, whose fixed prompt prefix a side call would
// break, or no reading to be had): the caller's own rule decides, as before.
func (o *OperatorSkill) replyAccepts(rc RunContext, st *operatorState, asked, reply string) (accepts, known bool) {
	reply = strings.Join(strings.Fields(reply), " ")
	if reply == "" {
		return false, true
	}
	if plainlyOf(reply, plainAcceptWords) && !refusalReplyRe.MatchString(reply) {
		return true, true
	}
	if plainlyOf(reply, plainRefuseWords) && refusalReplyRe.MatchString(reply) {
		return false, true
	}
	does, _, ok := o.readReply(rc, st, asked, reply)
	if !ok {
		return false, false
	}
	return does == "accepts", true
}

// replyWants is what the person's reply asks the worker to tell, show or do before it goes on ("which bill did you
// count as cancelled - tell me what it shows"), in their words, or "" when it asks nothing (or no reading is to be had).
// Found live (2026-10-23): told "not yet - tell me in 2-3 lines which invoice you treated as cancelled and what it
// shows", the worker was told "that settles what you asked" and asked for the approval again, three times.
func (o *OperatorSkill) replyWants(rc RunContext, st *operatorState, asked, reply string) string {
	reply = strings.Join(strings.Fields(reply), " ")
	if reply == "" || plainlyOf(reply, plainAcceptWords) || plainlyOf(reply, plainRefuseWords) {
		return ""
	}
	_, wants, _ := o.readReply(rc, st, asked, reply)
	return wants
}

// referenceNote is what the model is told when the person's reply relies on a practice it does not spell out ("you
// know how we treat cancelled ones"): find it where such things are kept and say where, or ask - never guess it. ""
// when the reply spells out what it relies on (or no reading is to be had).
func (o *OperatorSkill) referenceNote(rc RunContext, st *operatorState, asked, reply string) string {
	reply = strings.Join(strings.Fields(reply), " ")
	if reply == "" || plainlyOf(reply, plainAcceptWords) || plainlyOf(reply, plainRefuseWords) {
		return ""
	}
	if _, _, ok := o.readReply(rc, st, asked, reply); !ok || st.replyRead.refers == "" {
		return ""
	}
	return "\n\nThey rely on something they do not spell out: " + st.replyRead.refers + ". Find it where such things " +
		"are kept - what earlier tasks kept for you (the lessons and notes in your instructions), what the person taught " +
		"(gui skill, gui guide), and how earlier periods or records were done - and quote where you found it before you " +
		"act on it. If none of those holds it, ask them for it in one line, saying you have no record of it; never guess it."
}

// readReply is the model's one reading of a reply (cached per reply): what it does to what was asked, and what it
// asks of the worker. ok is false on a local brain, or when no reading is to be had.
func (o *OperatorSkill) readReply(rc RunContext, st *operatorState, asked, reply string) (does, wants string, ok bool) {
	if !st.Hosted || rc.Provider == nil {
		return "", "", false
	}
	if st.replyRead.reply == reply {
		return st.replyRead.does, st.replyRead.wants, true
	}
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: replySystem},
		{Role: "user", Content: "What they were asked: " + truncateText(asked, 3000) + "\n\nTheir reply: " + truncateText(reply, 3000)}},
		llm.Options{TimeoutMs: 60000, Temperature: 0, Purpose: llm.PurposeReplyRead})
	if err != nil {
		rc.Logf("operator", "could not read the person's reply: %v", err)
		return "", "", false
	}
	text := resp.Text
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	var answer struct {
		Does   string `json:"does"`
		Wants  string `json:"wants"`
		Refers string `json:"refers"`
	}
	if json.Unmarshal([]byte(text), &answer) != nil || answer.Does == "" {
		rc.Logf("operator", "the reading of the person's reply could not be read")
		return "", "", false
	}
	does = strings.ToLower(strings.TrimSpace(answer.Does))
	wants = truncateText(strings.Join(strings.Fields(answer.Wants), " "), 800)
	st.replyRead.reply, st.replyRead.does, st.replyRead.wants = reply, does, wants
	st.replyRead.refers = truncateText(strings.Join(strings.Fields(answer.Refers), " "), 300)
	rc.Logf("operator", "the person's reply %s what was asked", does)
	return does, wants, true
}
