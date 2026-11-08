package skills

import (
	"fmt"
	"regexp"
	"strings"
)

// A save refused by the same check, again and again, on a form that did not change. Found live (2026-11-08, a
// learning run): the agent's save check refused "Save bill" with the same "Not pressed: ... Notes holds ..." seven
// times and more, while the form held the same values; between the tries the worker declared the record again, typed
// the field again, fetched the page with curl and opened the browser's developer tools, until its budget was spent.
// (The check itself was wrong - it read a cut value - but a refusal the worker cannot clear is the person's to see,
// whatever its cause.) Counted here from the tool's own output: the gui command's exit code and the refusal line it
// prints - never the worker's words.

const (
	// maxSameSaveRefusals is how often the same refusal on an unchanged form may come back before the person is asked.
	maxSameSaveRefusals = 3
	// maxRefusalQuoteChars bounds the refusal quoted to the person.
	maxRefusalQuoteChars = 600
)

// saveRefusalRe is the line the gui command prints when the agent's checks before a save or a submit stopped it
// (computer/cli.py prints a refusal as "gui: <reason>"; core._commit_check's reasons start "Not pressed:"). "Not
// pressed yet" (a listing confirmed by the same click with --checked) and "Not pressed -" (a listing confirmed by the
// form's own control) are steps of the check, not refusals.
var saveRefusalRe = regexp.MustCompile(`(?m)^gui: (Not pressed: .+)$`)

// refusedFieldRe finds the first field a refusal names, as core's save check words it ("Notes holds '...'", "No. is
// empty where ...", "Cost centre is left unset ...") - the shape of the agent's own message, not words of a task.
var refusedFieldRe = regexp.MustCompile(`(?:says - |; )([^;:'"\n]{1,60}?) (?:holds '|is empty where |is left unset )`)

// saveRefusal is the refusal a command's output reports, or "" when the command was no gui action that the save check
// stopped (exit code 1 and the refusal line, on a command that acts on the screen).
func saveRefusal(command string, r shellResult) string {
	if r.ExitCode != 1 || !touchesScreen(command) {
		return ""
	}
	m := saveRefusalRe.FindStringSubmatch(r.Output)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// refusalKey is a refusal as compared with the one before: element refs (new at each listing) and spacing aside. The
// refusal quotes what each field it judges holds and what the record says, so the same key means the form's judged
// fields and the record are as they were: a field typed otherwise, or a record declared again with other values,
// gives another key.
func refusalKey(refusal string) string {
	return strings.Join(strings.Fields(elementRefRe.ReplaceAllString(refusal, "e#")), " ")
}

// noteSaveRefusal counts the same save refusal coming back. A refusal unlike the last one starts the count again;
// commands between refusals (a field typed again with the same value, a record declared again the same, a look) do not
// reset it - they changed nothing the check reads. At maxSameSaveRefusals, SaveRefusalAsk holds the question the loop
// puts to the person, and the count starts again for their answer.
func (st *operatorState) noteSaveRefusal(command string, r shellResult) {
	refusal := saveRefusal(command, r)
	if refusal == "" {
		return
	}
	key := refusalKey(refusal)
	if key != st.SaveRefusal {
		st.SaveRefusal, st.SaveRefusals = key, 0
	}
	st.SaveRefusals++
	if st.SaveRefusals >= maxSameSaveRefusals {
		st.SaveRefusals = 0
		st.SaveRefusalAsk = saveRefusalQuestion(refusal, maxSameSaveRefusals)
	}
}

// saveRefusalQuestion is what the person is asked: the refusal quoted, and the field it names.
func saveRefusalQuestion(refusal string, times int) string {
	field := "the field it names"
	if m := refusedFieldRe.FindStringSubmatch(refusal); m != nil {
		field = strings.TrimSpace(m[1])
	}
	return fmt.Sprintf("The save is refused by the same check every time - %d times now, with the form unchanged between "+
		"the tries - and I cannot clear it myself:\n\n%s\n\nThe field it names: %s. Tell me what %s should hold, or how "+
		"to go on (or reply `stop`).", times, truncateText(refusal, maxRefusalQuoteChars), field, field)
}
