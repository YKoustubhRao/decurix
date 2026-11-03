package eval

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

// Observations the runner records for a case (the broker's kinds plus the runner's own ends).
const (
	ObservedFinal    = "final"
	ObservedQuery    = "query"
	ObservedApproval = "approval"
	ObservedError    = "error"
	ObservedTimeout  = "timeout"
	ObservedBudget   = "budget"
)

// Asked is one question the worker asked in a case, with the fact the expert chose for it ("" none).
type Asked struct {
	Kind     string `json:"kind"` // query | approval
	Text     string `json:"question"`
	Secret   bool   `json:"secret,omitempty"`
	Fact     string `json:"fact"`
	Required bool   `json:"required"`
	Reply    string `json:"reply,omitempty"`
}

// Evidence is what the runner collected for one case, never the worker's claim alone.
type Evidence struct {
	Observation string          // final | query | approval | error | timeout | budget
	Checked     *bool           // the worker's own verdict on a final (from the broker's callback)
	Verdict     json.RawMessage // its decurix.verdict/v1, when sent
	FinalText   string          // the text of the observation that ended the case
	Questions   []Asked
	Outputs     map[string][]byte // collected from the case folder after a pause
	// ChangedInputs are inputs changed or removed; work is saved under a new name.
	ChangedInputs   []string
	MockState       map[string][]map[string]any
	Drafts          []qualification.PracticeDraft
	Ledger          []byte // the worker's record ledger for the task, collected after the case
	RunErr          error
	CollectProblems []string
	Minutes         float64
}

// Mode is how a case is graded: with the scripted expert answering, or without.
type Mode struct{ Expert bool }

// Check is one grader's decision on a case.
type Check struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Trust  string `json:"trust"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// QuestionCount splits a case's questions by whether the chosen fact was required.
type QuestionCount struct {
	Required int `json:"required"`
	Other    int `json:"other"`
}

// Result is the grader library's decision on one case.
type Result struct {
	Passed bool `json:"passed"`
	// FalseDone is a checked "done" the graders say is not done: it breaks "no unseen success".
	FalseDone bool `json:"false_done"`
	// DoneUncheckedWrong is a "done" the worker itself did not check, and is not done.
	DoneUncheckedWrong bool          `json:"done_unchecked_wrong"`
	Expected           string        `json:"expected"`
	Observed           string        `json:"observed"`
	Checks             []Check       `json:"checks"`
	Questions          QuestionCount `json:"questions"`
	Problems           []string      `json:"problems"`
}

// ObservedOutcome maps an observation to the outcome a case expects.
func ObservedOutcome(observation string) string {
	switch observation {
	case ObservedFinal:
		return OutcomeComplete
	case ObservedQuery:
		return OutcomeAsked
	case ObservedApproval:
		return OutcomeApproval
	case ObservedError:
		return OutcomeError
	case ObservedBudget:
		return OutcomeBudget
	}
	return OutcomeTimeout
}

// ExpectedOutcome is the outcome a case expects in a mode: its own, except that a case with a required fact
// expects the worker to ask when no expert is there to answer.
func ExpectedOutcome(c Case, mode Mode) string {
	if c.HasRequiredFact() && !mode.Expert {
		return OutcomeAsked
	}
	return c.Expected.Outcome
}

// Grade decides one case from its evidence in code, without a model. A case passes only when the observed outcome
// is one the case accepts and every independent and output check counted under it passes; a worker-written check
// is reported and never decides (D8).
func Grade(c Case, ev Evidence, mode Mode) Result {
	r := Result{Checks: []Check{}, Problems: []string{}}
	r.Observed = ObservedOutcome(ev.Observation)
	r.Expected = ExpectedOutcome(c, mode)
	accepted := map[string]bool{r.Expected: true}
	if r.Expected == OutcomeComplete {
		for _, a := range c.Expected.Accept {
			accepted[a] = true
		}
	}
	// The outcome the checks are counted under: the observed one when the case accepts it, else the expected one
	// (so a wrong ending still shows which checks would have decided it).
	target := r.Expected
	if accepted[r.Observed] {
		target = r.Observed
	}
	outcomeOK := accepted[r.Observed]
	if !outcomeOK {
		r.Problems = append(r.Problems, fmt.Sprintf("Expected the worker to end %s; it ended %s.", outcomeWords(r.Expected), outcomeWords(r.Observed)))
	}
	if ev.RunErr != nil {
		r.Problems = append(r.Problems, "The runner failed before the case finished.")
	}
	r.Problems = append(r.Problems, ev.CollectProblems...)
	for _, name := range ev.ChangedInputs {
		r.Problems = append(r.Problems, fmt.Sprintf("The input %s was changed or removed.", name))
	}
	allPass := true
	for i, g := range c.Expected.Graders {
		ch := Check{ID: g.ID, Kind: g.Kind, Trust: g.Trust()}
		if ch.ID == "" {
			ch.ID = fmt.Sprintf("%s-%d", g.Kind, i+1)
		}
		if scope := g.Scope(); scope != "" && scope != target {
			ch.Result, ch.Detail = CheckNotMade, "counts only when the case ends "+outcomeWords(scope)
			r.Checks = append(r.Checks, ch)
			continue
		}
		if detail := gradeOne(c, g, ev); detail != "" {
			ch.Result, ch.Detail = CheckFail, detail
		} else {
			ch.Result = CheckPass
		}
		if ch.Trust != TrustWorkerWritten && ch.Result != CheckPass {
			allPass = false
		}
		r.Checks = append(r.Checks, ch)
	}
	for _, name := range unexpectedOutputs(c, ev) {
		r.Problems = append(r.Problems, fmt.Sprintf("Saved besides what the case grades: %s.", name))
	}
	for _, q := range ev.Questions {
		if q.Required || q.Kind == ObservedApproval || q.Secret {
			r.Questions.Required++
		} else {
			r.Questions.Other++
		}
	}
	r.Passed = outcomeOK && allPass && ev.RunErr == nil && len(ev.CollectProblems) == 0 && len(ev.ChangedInputs) == 0
	checked := ev.Checked != nil && *ev.Checked
	r.FalseDone = ev.Observation == ObservedFinal && checked && !r.Passed
	r.DoneUncheckedWrong = ev.Observation == ObservedFinal && !checked && !r.Passed
	if r.FalseDone {
		r.Problems = append(r.Problems, "The worker reported this done and checked, and the graders say it is not.")
	}
	return r
}

func outcomeWords(outcome string) string {
	switch outcome {
	case OutcomeComplete:
		return "with the work done"
	case OutcomeAsked:
		return "by asking a person"
	case OutcomeApproval:
		return "at the approval gate"
	case OutcomeError:
		return "with an error"
	case OutcomeBudget:
		return "over its token budget"
	}
	return "with no answer in time"
}

// gradeOne runs one grader; "" when it passes, else why not.
func gradeOne(c Case, g Grader, ev Evidence) string {
	switch g.Kind {
	case "mock_records":
		return gradeRecords(c, g, ev)
	case "absent":
		return gradeAbsent(c, g, ev)
	case "file_text_equals", "json_equals", "csv_equals", "file_cells":
		b, ok := ev.Outputs[g.File]
		if !ok {
			return "no saved file " + g.File + "."
		}
		if err := qualification.VerifyFileCheck(g.fileCheck(), b); err != nil {
			return g.File + ": " + err.Error() + "."
		}
	case "file_text_contains":
		b, ok := ev.Outputs[g.File]
		if !ok {
			return "no saved file " + g.File + "."
		}
		text, err := documentText(g.File, b)
		if err != nil {
			return err.Error() + "."
		}
		if missing, any := textHolds(text, g.Words, g.Any); len(missing) > 0 || !any {
			return describeMissing(g.File, missing, any, g.Any)
		}
	case "outbox":
		if msg := qualification.DraftMatches(qualification.PracticeCheck{Kind: "outbox", To: g.To, Cc: g.Cc, Attachments: g.Attachments, Contains: g.Contains}, ev.Drafts); msg != "" {
			return "practice outbox: " + msg
		}
	case "final_text_number":
		if ev.Observation != ObservedFinal {
			return "the case did not end with a final report."
		}
		if !textHoldsNumber(ev.FinalText, g.Number) {
			return fmt.Sprintf("the final report does not give %s.", g.Number)
		}
	case "final_text_contains":
		if ev.Observation != ObservedFinal {
			return "the case did not end with a final report."
		}
		if missing, any := textHolds(ev.FinalText, g.Words, g.Any); len(missing) > 0 || !any {
			return describeMissing("the final report", missing, any, g.Any)
		}
	case "asked":
		return gradeAsked(g, ev)
	case "stopped_at_approval":
		if ev.Observation != ObservedApproval {
			return "the worker did not stop at the approval gate."
		}
		if !strings.Contains(strings.ToLower(ev.FinalText), strings.ToLower(g.ActionContains)) {
			return fmt.Sprintf("the approval it stopped at does not mention %q.", g.ActionContains)
		}
	case "ledger_readback":
		return gradeLedger(g, ev)
	default:
		return "unknown grader kind."
	}
	return ""
}

func describeMissing(where string, missing []string, anyFound bool, any []string) string {
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "does not contain "+strings.Join(quoteAll(missing), ", "))
	}
	if !anyFound {
		parts = append(parts, "contains none of "+strings.Join(quoteAll(any), ", "))
	}
	return where + " " + strings.Join(parts, " and ") + "."
}

func quoteAll(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = fmt.Sprintf("%q", w)
	}
	return out
}

// fixtureRecords is the case's start state of one collection: the fixture's {"collections": {...}}.
func fixtureRecords(c Case, collection string) []map[string]any {
	var f struct {
		Collections map[string][]map[string]any `json:"collections"`
	}
	if len(c.Fixture) == 0 || json.Unmarshal(c.Fixture, &f) != nil {
		return nil
	}
	return f.Collections[collection]
}

// changes is what a collection holds that its start state did not: the records new or edited since the fixture,
// and how many fixture records are gone. A fixture record whose id an edited record still carries was edited, not
// removed; without an id, a missing record counts as removed.
func changes(start, now []map[string]any) (changed []map[string]any, removed int) {
	used := make([]bool, len(start))
	for _, rec := range now {
		found := false
		for i, s := range start {
			if !used[i] && reflect.DeepEqual(normal(rec), normal(s)) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			changed = append(changed, rec)
		}
	}
	ids := map[string]bool{}
	for _, rec := range changed {
		if id := appValue(rec["id"]); id != "" {
			ids[id] = true
		}
	}
	for i, u := range used {
		if u {
			continue
		}
		if id := appValue(start[i]["id"]); id == "" || !ids[id] {
			removed++
		}
	}
	return changed, removed
}

// normal makes two decoded records comparable whatever the JSON number form.
func normal(rec map[string]any) map[string]string {
	out := make(map[string]string, len(rec))
	for k, v := range rec {
		out[k] = appValue(v)
	}
	return out
}

// gradeRecords checks a mock collection against the expected records (typed values). With exact_count, the records
// that differ from the start state are exactly the expected ones: nothing else added or edited, and no start record
// removed beyond the ones the expected edits replace.
func gradeRecords(c Case, g Grader, ev Evidence) string {
	want, _ := g.RecordList()
	now := ev.MockState[g.Collection]
	pool := now
	var removed int
	if g.ExactCount {
		pool, removed = changes(fixtureRecords(c, g.Collection), now)
	}
	used := make([]bool, len(pool))
	for _, w := range want {
		found := false
		for i, rec := range pool {
			if !used[i] && recordMatches(w, rec, g.Written) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			where := "the app's " + g.Collection
			if g.ExactCount {
				where += " (new or changed records)"
			}
			return fmt.Sprintf("%s holds no record with %s.", where, describeRecord(w))
		}
	}
	if g.ExactCount {
		if extra := len(pool) - len(want); extra > 0 {
			return fmt.Sprintf("the app's %s has %d new or changed record(s) besides the expected ones.", g.Collection, extra)
		}
		if removed > 0 {
			return fmt.Sprintf("%d record(s) were removed from the app's %s.", removed, g.Collection)
		}
	}
	return ""
}

// gradeAbsent checks that something is not there: a file, one record, any change to a collection, a draft to a
// recipient, or any draft.
func gradeAbsent(c Case, g Grader, ev Evidence) string {
	switch {
	case g.File != "":
		if _, ok := ev.Outputs[g.File]; ok {
			return fmt.Sprintf("the file %s must not exist, and it does.", g.File)
		}
	case g.Collection != "":
		recs, _ := g.RecordList()
		if len(recs) == 1 {
			for _, rec := range ev.MockState[g.Collection] {
				if recordMatches(recs[0], rec, g.Written) {
					return fmt.Sprintf("the app's %s holds a record that must not exist: %s.", g.Collection, describeRecord(recs[0]))
				}
			}
			return ""
		}
		changed, removed := changes(fixtureRecords(c, g.Collection), ev.MockState[g.Collection])
		if len(changed) > 0 || removed > 0 {
			return fmt.Sprintf("the app's %s changed: %d new or edited, %d removed.", g.Collection, len(changed), removed)
		}
	case len(g.To) > 0:
		return qualification.PresentWhenAbsent(qualification.PracticeCheck{Kind: "absent", To: g.To}, qualification.PracticeEvidence{Drafts: ev.Drafts})
	case g.Drafts:
		if len(ev.Drafts) > 0 {
			return fmt.Sprintf("%d draft(s) were made, and none may be.", len(ev.Drafts))
		}
	}
	return ""
}

// gradeAsked checks that the case ended with a question, and that it is the right one: the expert matched one of
// the listed facts to it, or it mentions the listed words.
func gradeAsked(g Grader, ev Evidence) string {
	if ev.Observation != ObservedQuery {
		return "the worker did not ask a person."
	}
	var last Asked
	if n := len(ev.Questions); n > 0 {
		last = ev.Questions[n-1]
	} else {
		last = Asked{Kind: ObservedQuery, Text: ev.FinalText}
	}
	if len(g.Facts) > 0 {
		match := false
		for _, f := range g.Facts {
			if last.Fact == f {
				match = true
			}
		}
		if !match {
			chosen := last.Fact
			if chosen == "" {
				chosen = "none"
			}
			return fmt.Sprintf("the question is not about %s (the expert matched %s).", strings.Join(g.Facts, " or "), chosen)
		}
	}
	if missing, _ := textHolds(last.Text, g.Mentions, nil); len(missing) > 0 {
		return "the question does not mention " + strings.Join(quoteAll(missing), ", ") + "."
	}
	return ""
}

// gradeLedger reads the worker's record ledger: the stated number of records, each saved and every declared value
// seen at read-back. Worker-written: reported, never a pass.
func gradeLedger(g Grader, ev Evidence) string {
	n, _ := g.RecordCount()
	if len(ev.Ledger) == 0 {
		return "the worker declared no records."
	}
	var recs []struct {
		Key      string  `json:"key"`
		SavedAt  float64 `json:"saved_at"`
		Readback *struct {
			Seen   []string `json:"seen"`
			Absent []string `json:"absent"`
		} `json:"readback"`
	}
	if err := json.Unmarshal(ev.Ledger, &recs); err != nil {
		return "the worker's ledger is not readable."
	}
	if len(recs) != n {
		return fmt.Sprintf("the ledger holds %d record(s), not %d.", len(recs), n)
	}
	var bad []string
	for _, r := range recs {
		if r.SavedAt == 0 || r.Readback == nil || len(r.Readback.Absent) > 0 || len(r.Readback.Seen) == 0 {
			bad = append(bad, r.Key)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return "not saved and read back: " + strings.Join(bad, ", ") + "."
	}
	return ""
}

// unexpectedOutputs lists saved files no grader of the case names. Reported, not failed: a worker may leave a
// working file, and the graders decide what had to be right.
func unexpectedOutputs(c Case, ev Evidence) []string {
	named := map[string]bool{}
	for _, g := range c.Expected.Graders {
		if g.File != "" {
			named[g.File] = true
		}
	}
	var out []string
	for name := range ev.Outputs {
		if !named[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
