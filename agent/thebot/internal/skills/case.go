package skills

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"thebot/internal/llm"
)

// ============================================================
// The case matrix (docs/learning-loop.md, phase A). Before an action that cannot be taken back, the records it covers
// are listed against every source the work read: present, absent, or what the source says about each. A record the
// sources disagree about - on file in one place, left out of another; marked one way here and another there - needs an
// explanation a source holds (quoted, and found in that source's own text) or the person's decision. One without is a
// point for the worker to settle before the action, and for the person after. The model lists the records and quotes;
// code checks every quote against what was read. Nothing here knows any domain.
//
// Found live (2026-10-23): a bill on file was left out of the register the request called up to date, and the payment
// was made on figures without it; nothing listed that, and the record was decided by whoever guessed.
// ============================================================

const caseSystem = `You audit, independently, which records an action covers, before it is taken. You are given what the worker was asked, what the person said, their rules and what they taught, the sources the work read (documents, sheet rows) and the screen the action commits.

1. List the records of the kind that screen reports or commits, each by its key as the sources write it (a document number, a reference, a row's id): every one any source mentions within the scope of the work.
2. For each, say what each source says about it, by the source's name as given: "present" (it is there, and as what), "absent", or what the source says about it ("marked cancelled", "a corrected copy of 452", "paid"). "Absent" only from a source that lists records of this kind for the scope (a register, a summary, a ledger, a statement, a folder) - a document that is one record (one bill, one letter, one photo of a bill) says nothing about any other record: leave it out for them. Leave out sources that are not about records of this kind.
3. Say whether the screen about to be committed includes it: "in", "out" or "unclear".
4. Say whether the sources disagree about it - one has it and another that lists such records does not, or they say different things. When they do, quote the exact words of a source (never the screen being committed) that explain why, with that source's name; leave the quote empty when nothing you were given explains it. Never explain a disagreement yourself.

Reply with JSON only: {"records": [{"key": "...", "sources": {"<source name>": "..."}, "outcome": "in" | "out" | "unclear", "disagree": true | false, "explained_by": {"source": "...", "quote": "..."}}]}`

const (
	maxCaseFindings    = 6
	maxCaseScreenChars = 20_000
	// minExplainWords is the shortest quote that can explain a disagreement: one word ("cancelled") stands in too many
	// places (a column's header) to say anything about one record.
	minExplainWords = 3
)

// caseRecord is one record of the matrix, as the model lists it.
type caseRecord struct {
	Key       string            `json:"key"`
	Sources   map[string]string `json:"sources"`
	Outcome   string            `json:"outcome"`
	Disagree  bool              `json:"disagree"`
	Explained struct {
		Source string `json:"source"`
		Quote  string `json:"quote"`
	} `json:"explained_by"`
}

// caseMatrix is the records the sources disagree about with nothing read that says why, as recheck points before the
// action; none when there is no screen of the action's window to read, and a "not checked" point when the check cannot be
// made (an error is never a pass).
func (o *OperatorSkill) caseMatrix(rc RunContext, st *operatorState, action string) []reviewIssue {
	if rc.Provider == nil || !st.Hosted {
		return nil
	}
	listing := st.actionListing(action)
	if strings.TrimSpace(listing) == "" {
		return nil
	}
	facts := llm.Message{Role: "user", Content: st.commitSources() + st.otherScreens(action) +
		"\nTHE ACTION: " + truncateText(action, 600) +
		"\n\nTHE SCREEN ABOUT TO BE COMMITTED:\n" + truncateText(listing, 20_000) + "\n"}
	facts.Images, facts.ImageLabels = st.documentPicturesKept(3)
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: caseSystem}, facts},
		llm.Options{TimeoutMs: verifyTimeoutMs, Temperature: reviewTemperature})
	if err != nil {
		rc.Logf("operator", "could not list the records before the action: %v", err)
		return []reviewIssue{notChecked("the records the action covers were not set against every source read", err)}
	}
	if resp.Truncated {
		rc.Logf("operator", "the record list before the action was cut off")
		return []reviewIssue{notChecked("the records the action covers were not set against every source read", errCutOff)}
	}
	text := resp.Text
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	var answer struct {
		Records []caseRecord `json:"records"`
	}
	if json.Unmarshal([]byte(text), &answer) != nil {
		rc.Logf("operator", "the record list before the action could not be read")
		return []reviewIssue{notChecked("the records the action covers were not set against every source read", nil)}
	}
	out := st.unexplained(answer.Records, action)
	rc.Logf("operator", "the case before the action: %d record(s), %d the sources disagree about with nothing read that says why",
		len(answer.Records), len(out))
	return out
}

// unexplained is the records of a matrix the sources disagree about whose explanation is no source's own words: a
// quote of fewer than minExplainWords words, or one found in no document, sheet row, other window's screen, request or
// reply of the person.
func (st *operatorState) unexplained(records []caseRecord, action string) []reviewIssue {
	var out []reviewIssue
	seen := map[string]bool{}
	for _, r := range records {
		key := strings.TrimSpace(r.Key)
		if !r.Disagree || key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := st.quotedBySource(r.Explained.Quote, action); ok {
			continue
		}
		out = append(out, reviewIssue{
			What: fmt.Sprintf("The sources disagree about %s (%s), and nothing read says why", key, sourcesSay(r.Sources)),
			Why: fmt.Sprintf("the action has it %s - find what explains it (a document, a message, a register) or ask "+
				"the person, before the action", outcomeWords(r.Outcome)),
			Settle: "recheck",
		})
		if len(out) == maxCaseFindings {
			break
		}
	}
	return out
}

// quotedBySource is the source whose own text holds a quote of at least minExplainWords words - a document, a sheet
// row, another window's screen, the request or the person's words, never the window being committed nor a command's
// working - and whether one does.
func (st *operatorState) quotedBySource(quote, action string) (string, bool) {
	if len(strings.Fields(plainWords(quote))) < minExplainWords {
		return "", false
	}
	for i := len(st.Units) - 1; i >= 0; i-- {
		u := st.Units[i]
		if !sourceKind(u.Kind) && u.Kind != "person" && u.Kind != "request" && u.Kind != "memory" && !st.otherScreen(u, action) {
			continue
		}
		if quoteSeen(quote, " "+plainWords(u.Text)+" ") {
			return u.Source, true
		}
	}
	return "", false
}

// otherScreen reports whether a unit is a line read off a window other than the one the action acts on (a known
// window: lines whose window is unknown are not taken as sources).
func (st *operatorState) otherScreen(u evidenceUnit, action string) bool {
	committed := actionWindow(action)
	if u.Kind != "screen" || committed == "" || !strings.HasPrefix(u.Source, screenSourcePrefix) {
		return false
	}
	return !sameWindow(strings.TrimSuffix(strings.TrimPrefix(u.Source, screenSourcePrefix), "\""), committed)
}

// otherScreens is the lines the work read off windows other than the one being committed (a chat, a ledger, another
// page), newest first within maxCaseScreenChars, as a section of a check's facts - or "".
func (st *operatorState) otherScreens(action string) string {
	var b strings.Builder
	used := 0
	for i := len(st.Units) - 1; i >= 0 && used < maxCaseScreenChars; i-- {
		u := st.Units[i]
		if !st.otherScreen(u, action) {
			continue
		}
		piece := fmt.Sprintf("--- %s\n%s\n", u.Source, u.Text)
		used += len(piece)
		b.WriteString(piece)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\nOTHER SCREENS THE WORK READ (not the one being committed):\n" + b.String()
}

// sourcesSay is what each source says about a record, in one line ("SALES sheet: absent; Invoice_455.pdf: present").
func sourcesSay(sources map[string]string) string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, truncateText(name, 80)+": "+truncateText(strings.Join(strings.Fields(sources[name]), " "), 120))
	}
	return truncateText(strings.Join(parts, "; "), 600)
}

// outcomeWords says what the action does with a record: "in", "out", or "unclear" as words.
func outcomeWords(outcome string) string {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "in":
		return "in"
	case "out":
		return "left out"
	}
	return "neither clearly in nor out"
}
