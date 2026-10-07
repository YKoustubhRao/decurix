package skills

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"thebot/internal/llm"
)

// A value the operator types into an app must come from something it has
// seen: the request, or the output of a command it ran. Found live, on a
// hosted brain: asked to enter a customer's PO (a PDF named in the request)
// as a quotation, the operator's first command was
//
//	gui do enter-a-customer-po --param customer="Green Furnishings" ...
//
// It never read the PDF. The PO was Gemini Furniture's, with other products
// and quantities; the reference GF-5522 was the only true value, and the
// customer was invented to fit its initials. The replay typed the guesses
// faithfully, and the final check saw a saved quotation and called it done.
//
// So, before a command that types values runs (gui do's --param values, gui
// type's text, gui select's option):
//   - every document the request names must have been read by some earlier
//     command (its path appears in one), and
//   - each value must appear in what has been seen - unless a document page
//     was shown as a picture, whose words are not text this can search.
//
// A refusal says what is missing; after maxUntracedNudges the operator asks
// the person instead of trying again. Deterministic, like the rest of the
// operator's guards: the model's judgement is not what decides this.

// maxUntracedNudges bounds the refusals before the person is asked.
const maxUntracedNudges = 2

// maxSeenChars bounds what is kept of the request and command outputs.
const maxSeenChars = 400_000

var (
	// documentPathRe is a document named by its path in a request.
	// A Windows worker's documents are named C:\...\po.pdf.
	documentPathRe = regexp.MustCompile(`(?i)(?:~|/|\b[a-z]:[\\/])[\w.~/\\-]*[\w-]\.(?:pdf|xlsx|xlsm|xls|ods|docx|doc|odt|csv|eml|png|jpe?g|tiff?)\b`)
	// paramRe is one --param NAME=VALUE of gui do or gui replay.
	paramRe = regexp.MustCompile(`--param\s+[\w.-]+=("(?:[^"\\]|\\.)*"|'[^']*'|\S+)`)
)

// attachedFilesMark opens the lines of a request that name the files a
// person attached to it, one path a line (the control plane's
// attachmentsMark, the desktop agent's attachments.MARK).
const attachedFilesMark = "Files attached to this request, saved on this desktop:"

// requestDocuments returns the documents a request names: paths with a
// document's extension anywhere in it, and every file attached to it,
// whatever its kind - a person attaches a file for it to be read.
func requestDocuments(request string) []string {
	docs := documentPathRe.FindAllString(request, -1)

	if _, rest, found := strings.Cut(request, attachedFilesMark); found {
		for _, line := range strings.Split(rest, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			known := false
			for _, doc := range docs {
				known = known || doc == line
			}
			if !known {
				docs = append(docs, line)
			}
		}
	}

	return docs
}

// noteSeen adds text the operator has now seen (the request, a command's
// output, a person's reply), keeping the most recent maxSeenChars.
func (st *operatorState) noteSeen(text string) {
	st.Seen += "\n" + text
	if len(st.Seen) > maxSeenChars {
		st.Seen = st.Seen[len(st.Seen)-maxSeenChars:]
	}
}

// typedValues returns the values a command would type into an app: gui do's
// and gui replay's --param values (each item of a "; " list), and the text
// of gui type or the option of gui select. Nil for any other command.
func typedValues(command string) []string {
	words := shellWords(command)
	if len(words) < 2 || words[0] != "gui" {
		return nil
	}

	var raw []string
	switch words[1] {
	case "do", "replay":
		for _, m := range paramRe.FindAllStringSubmatch(command, -1) {
			raw = append(raw, unquote(m[1]))
		}
	case "type", "select":
		args := positional(words[2:])
		if len(args) > 0 {
			raw = append(raw, args[len(args)-1])
		}
	default:
		return nil
	}

	var out []string
	for _, v := range raw {
		for _, item := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == '\n' }) {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

// groundingNudge is what to tell the operator instead of running command, or
// "" when every value it would type can be traced.
func groundingNudge(st *operatorState, command string) string {
	values := typedValues(command)
	if len(values) == 0 {
		return ""
	}

	var unread []string
	for _, doc := range requestDocuments(st.Request) {
		if !readBefore(st.Commands, doc) {
			unread = append(unread, doc)
		}
	}
	if len(unread) > 0 {
		return fmt.Sprintf("Not run: the request names %s, and you have not read it. Every value you type must come "+
			"from it. Read it first (office read %s), then type what it says.", strings.Join(unread, ", "), unread[0])
	}

	if st.ShownPages {
		return "" // read from a picture: the words are not text to search
	}
	seen := normalise(st.Seen)
	var numbers map[string]bool
	var dates *dateSet
	var untraced []string
	for _, v := range values {
		if strings.Contains(seen, normalise(v)) || strings.Contains(seen, normalise(withoutNewlineMarks(v))) {
			continue
		}
		if n, ok := canonicalNumber(v); ok {
			if numbers == nil {
				numbers = numbersIn(st.Seen)
			}
			if numbers[n] {
				continue
			}
		}
		if d, ok := canonicalDate(v); ok {
			if dates == nil {
				dates = datesIn(st.Seen)
			}
			if dates.holds(d) {
				continue
			}
		}
		untraced = append(untraced, fmt.Sprintf("%q", v))
	}
	if len(untraced) == 0 {
		return capitalsNudge(st.Seen, values)
	}
	return fmt.Sprintf("Not run: %s appear nowhere in the request or in anything a command has shown you, so they "+
		"would be guesses. Find each value in the document or the page it comes from and use it exactly as written "+
		"there. A date may be typed in the format its field takes. If you converted one otherwise (a unit), or it cannot be found, ask with HELP instead.",
		strings.Join(untraced, ", "))
}

// withoutNewlineMarks is a typed value without the \n that gui type reads as
// "press Enter" - unless the value is a Windows path, where \n is the start
// of a folder's name.
func withoutNewlineMarks(v string) string {
	return strings.TrimSpace(strings.ReplaceAll(v, `\n`, ""))
}

// capitalsNudge asks for a value typed in capitals to be typed the way the
// document otherwise writes it. Found live: a PO's letterhead read "GEMINI
// FURNITURE" and its signature line "Gemini Furniture"; the operator typed the
// letterhead and the quotation was saved for a customer spelled like no
// record. A value only ever written in capitals (a code) is left alone.
func capitalsNudge(seen string, values []string) string {
	var fixes []string
	for _, v := range values {
		if w := asWritten(seen, v); w != v {
			fixes = append(fixes, fmt.Sprintf("%q as %q", v, w))
		}
	}
	if len(fixes) == 0 {
		return ""
	}
	return fmt.Sprintf("Not run: type %s. The document writes it in capitals only as a heading; records "+
		"are named the way the running text writes them. Send the command again with that spelling.",
		strings.Join(fixes, ", "))
}

// asWritten is seen's mixed-case spelling of an all-capitals value, or the
// value itself when there is none.
func asWritten(seen, value string) string {
	letters := 0
	for _, r := range value {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 2 || value != strings.ToUpper(value) {
		return value
	}
	var parts []string
	for _, w := range strings.Fields(value) {
		parts = append(parts, regexp.QuoteMeta(w))
	}
	re, err := regexp.Compile(`(?i)(?:^|[^\p{L}\p{N}_])(` + strings.Join(parts, `\s+`) + `)(?:$|[^\p{L}\p{N}_])`)
	if err != nil {
		return value
	}
	for _, m := range re.FindAllStringSubmatch(seen, -1) {
		if found := strings.Join(strings.Fields(m[1]), " "); found != strings.ToUpper(found) {
			return found
		}
	}
	return value
}

// earlyDoneNudge is what to tell the operator instead of taking a DONE sent
// before any command ran, or "" to take it. Found live, on a hosted brain:
// asked to review a supplier's price list, the operator's first reply was
// "DONE: Entered the PO as a quotation in the sales app..." - word for word
// the answer to the request before it, which the session history carried -
// and the job ended in a second with nothing done. A DONE with no command is
// still right for a request that needs none ("reply pong"); it is pushed
// back once when it repeats an earlier answer, or when the request names a
// document nothing has read.
var alreadyDoneRe = regexp.MustCompile(`(?i)\balready\b|\bearlier in this (session|conversation|chat)\b|\bpreviously\b|\bas before\b`)

func earlyDoneNudge(st *operatorState, history []llm.Message, answer string) string {
	said := normalise(answer)
	for _, m := range history {
		if m.Role != "assistant" || said == "" {
			continue
		}
		earlier := normalise(m.Content)
		if earlier == said || (len(said) >= 40 && strings.Contains(earlier, said)) {
			return "Not taken: that is your answer to an EARLIER request in this conversation, and nothing has " +
				"been run for this one. This request is new - do what it asks:\n" + st.Request
		}
	}
	// "Already signed in, verified earlier in this session": what was true
	// earlier in the conversation is not what is true now. Found by
	// bench/native: the same request sent again after the page had been
	// reset was answered from the history, with nothing looked at.
	if alreadyDoneRe.MatchString(answer) {
		return "Not taken: nothing has been looked at for this request. What you did or saw earlier in this " +
			"conversation may no longer hold - the screen and the files may have changed since. Look first " +
			"(gui observe, or the command that shows it), then do what is asked:\n" + st.Request
	}
	for _, doc := range requestDocuments(st.Request) {
		if !readBefore(st.Commands, doc) {
			return fmt.Sprintf("Not taken: the request names %s and nothing has read it or done anything yet. "+
				"Do the task first (office read %s to start), and answer DONE only with what the commands showed.", doc, doc)
		}
	}
	return ""
}

var (
	// What a request asks to be done in a window, by the kind of gui
	// command that does it.
	// "press Enter" and "write a report" are not typing into a window;
	// "enter the name into the form" is.
	asksToTypeRe  = regexp.MustCompile(`(?i)\b(type|types|typing|fill\s+(?:in|out))\b|\b(enter|write|put)\b[^.\n]{0,60}\b(into|in\s+the|in\s+it)\b`)
	asksToPressRe = regexp.MustCompile(`(?i)\b(click|press|tap|tick|untick|uncheck)\b`)
	guiCommandRe  = regexp.MustCompile(`(?m)(?:^|[;|&(]\s*)gui\s+([a-z]+)`)
	// requestQuoteRe is something a request quotes: words to type or a
	// control's name, not what it asks to be done.
	requestQuoteRe = regexp.MustCompile("\"[^\"\n]*\"|'[^'\n]*'|`[^`\n]*`|“[^”\n]*”")
)

// unperformedScreenNudge is what to tell the operator instead of taking a DONE
// for work in a window that no command did, or "" to take it. Found live on a
// native Windows worker with a 1.5B model: asked to "open notepad and type
// hello world into it", it ran `gui open notepad` and reported "Notepad
// opened and Hello World typed" - nothing had been typed. With no hosted
// model there is nothing to judge the screen afterwards (verify.py), so the
// commands themselves are the evidence: a request that says to type needs a
// command that types, one that says to click needs one that clicks or
// presses. Only for a goal that is already on the screen (some gui command
// ran), and a taught skill or a replay (gui do, gui replay) may have done
// either.
func unperformedScreenNudge(st *operatorState) string {
	// Only what worked counts as done: a `gui type` the screen layer refused
	// (aimed at a button) typed nothing. What was merely tried still says
	// the goal is on the screen.
	onScreen := false
	for _, c := range st.Commands {
		onScreen = onScreen || guiCommandRe.MatchString(c)
	}
	ran := map[string]bool{}
	for _, c := range st.Worked {
		for _, m := range guiCommandRe.FindAllStringSubmatch(c, -1) {
			ran[m[1]] = true
		}
	}
	if !onScreen || ran["do"] || ran["replay"] {
		return ""
	}
	// The request with what it quotes taken out: "type 'click here'" asks
	// for typing only.
	asked := requestQuoteRe.ReplaceAllString(st.Request, " ")
	switch {
	case asksToTypeRe.MatchString(asked) && !ran["type"] && !ran["select"]:
		return "Not taken: the task says to type something, and no command has typed anything yet (gui open only opens the window). " +
			"Run gui observe to find the field's ref, then gui type REF \"the text\". Answer DONE only after a gui type printed what changed.\n\n" +
			"Reply now with exactly:\nCOMMAND:\ngui observe"
	case asksToPressRe.MatchString(asked) && !ran["click"] && !ran["key"] && !ran["select"]:
		return "Not taken: the task says to click or press something, and no command has done that yet. " +
			"Run gui observe to find the control's ref, then gui click REF (or gui key for a key). Answer DONE only after it printed what changed.\n\n" +
			"Reply now with exactly:\nCOMMAND:\ngui observe"
	}
	return ""
}

// observedFieldRe is a line of gui observe's listing that is a field text
// goes into: `e28 document "Text editor"`, `e2 textbox "Customer" value="..."`.
var observedFieldRe = regexp.MustCompile(`(?m)^(e\d+) (textbox|document|searchbox|entry|password text)\b([^\n]*)$`)

// typingFieldHint follows a gui observe that came after the push back above,
// for a task that says to type: it names the fields in the listing and the
// command that types into one. Found live with a 1.5B model: told to observe
// and then type, it ran gui observe, was shown thirty elements of Notepad
// (the editor among two dozen toolbar buttons), and answered DONE again -
// and, asked once more by the person, typed into the Strikethrough button.
// Only after the model has already claimed the typing once without doing it;
// a model that works the screen by itself is told nothing.
func typingFieldHint(st *operatorState, command, output string) string {
	if !strings.HasPrefix(strings.TrimSpace(command), "gui observe") ||
		!asksToTypeRe.MatchString(requestQuoteRe.ReplaceAllString(st.Request, " ")) {
		return ""
	}
	var fields []string
	for _, m := range observedFieldRe.FindAllStringSubmatch(output, -1) {
		if strings.Contains(m[3], "disabled") {
			continue
		}
		fields = append(fields, strings.TrimSpace(m[0]))
		if len(fields) == 3 {
			break
		}
	}
	if len(fields) == 0 {
		return ""
	}
	ref := strings.Fields(fields[0])[0]
	return "\n\nNothing has been typed yet. The fields text can be typed into are:\n" + strings.Join(fields, "\n") +
		"\n\nType into the right one now, with the text the task gives:\nCOMMAND:\ngui type " + ref + " \"the text\""
}

// unperformedScreenDoubt is what a person is asked when the operator says
// done a second time and still no command did the work: the report is theirs
// to judge, never passed on as a result. (The rule this follows: a worker may
// ask, or say it failed, but never reports a success nothing showed.)
func unperformedScreenDoubt(nudge, claim string) string {
	missing := "no command typed anything"
	if strings.Contains(nudge, "click or press") {
		missing = "no command clicked or pressed anything"
	}
	return fmt.Sprintf("I cannot confirm this is done: the task asks for it, but %s. My own report was: %q. "+
		"Look at the screen and reply `done` if it is, or tell me what is still missing.", missing, claim)
}

// readBefore reports whether an earlier command named the document (by its
// full path, or its file name when it was reached by a relative path).
func readBefore(commands []string, doc string) bool {
	base := doc[strings.LastIndexAny(doc, `/\`)+1:]
	for _, c := range commands {
		if strings.Contains(c, doc) || strings.Contains(c, base) {
			return true
		}
	}
	return false
}

// numberRe is a number as documents write it: digits with thousands separators (1,88,800 or 188,800) and decimals.
var numberRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// canonicalNumber is a value that is only a number - an amount, a rate, a quantity, with any currency mark or
// separators - written one way (6,000.00, Rs. 6000 and 6000 are all "6000"), or false.
func canonicalNumber(v string) (string, bool) {
	// Any currency mark before the digits: a symbol (₹ $ € £ ¥) or a code or abbreviation (Rs., INR, USD, EUR, Fr.).
	v = strings.TrimLeftFunc(strings.TrimSpace(v), func(r rune) bool {
		return !unicode.IsDigit(r) && (unicode.IsLetter(r) || unicode.IsSymbol(r) || unicode.IsSpace(r) || r == '.')
	})
	if !numberRe.MatchString(v) || numberRe.FindString(v) != v {
		return "", false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(f, 'f', -1, 64), true
}

// numbersIn is every number written in text, each in canonicalNumber's form. A bill writes a rate as "Rs. 6,000.00"
// and the form it goes into takes 6000: found live, the plain text match refused the chair's rate on an ERP bill and
// the operator stopped to ask whether to type the bill's own figure.
func numbersIn(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range numberRe.FindAllString(text, -1) {
		if n, ok := canonicalNumber(m); ok {
			out[n] = true
		}
	}
	return out
}

// dateRe is a date as documents and forms write it: 04-10-2026, 4/10/2026, 2026-10-04, 04-Oct-2026, 4 October 2026,
// October 4, 2026.
var dateRe = regexp.MustCompile(`(?i)\b(?:` +
	`(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})` +
	`|(\d{1,2})[-/. ](\d{1,2})[-/. ](\d{4})` +
	`|(\d{1,2})(?:st|nd|rd|th)?[-/. ]+([a-z]{3,9})\.?[-/., ]+(\d{4})` +
	`|([a-z]{3,9})\.?[ ]+(\d{1,2})(?:st|nd|rd|th)?,?[ ]+(\d{4})` +
	`)\b`)

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12}

// monthOf is a month's number from its name or abbreviation, or 0.
func monthOf(name string) int {
	name = strings.ToLower(name)
	if m, ok := monthNames[name]; ok {
		return m
	}
	if len(name) > 3 {
		if m, ok := monthNames[name[:3]]; ok {
			return m
		}
	}
	return 0
}

// dateValue is a date as written. days holds what it surely means (yyyy-mm-dd): a month named, the year first, or
// all digits where one field is past 12 or the two are equal. An all-digit date whose first two fields could be
// either day or month (04-10-2026) is ambiguous: days is empty and order keeps the fields as written.
type dateValue struct {
	days  []string
	order string
}

// dateOf is what a match of dateRe means.
func dateOf(m []string) dateValue {
	day := func(y, mo, d int) (string, bool) {
		if mo < 1 || mo > 12 || d < 1 || d > 31 {
			return "", false
		}
		return fmt.Sprintf("%04d-%02d-%02d", y, mo, d), true
	}
	atoi := func(x string) int { n, _ := strconv.Atoi(x); return n }
	var v dateValue
	add := func(y, mo, d int) {
		if s, ok := day(y, mo, d); ok {
			v.days = append(v.days, s)
		}
	}
	switch {
	case m[1] != "": // year first
		add(atoi(m[1]), atoi(m[2]), atoi(m[3]))
	case m[6] != "": // all digits
		a, b, y := atoi(m[4]), atoi(m[5]), atoi(m[6])
		switch {
		case a > 12 || a == b:
			add(y, b, a)
		case b > 12:
			add(y, a, b)
		default:
			v.order = fmt.Sprintf("%d/%d/%d", a, b, y)
		}
	case m[9] != "": // 4 October 2026
		if mo := monthOf(m[8]); mo > 0 {
			add(atoi(m[9]), mo, atoi(m[7]))
		}
	case m[12] != "": // October 4, 2026
		if mo := monthOf(m[10]); mo > 0 {
			add(atoi(m[12]), mo, atoi(m[11]))
		}
	}
	return v
}

// canonicalDate is a value that is only a date, read as dateOf reads it, or false.
func canonicalDate(v string) (dateValue, bool) {
	v = strings.TrimSpace(v)
	loc := dateRe.FindStringSubmatchIndex(v)
	if loc == nil || loc[0] != 0 || loc[1] != len(v) {
		return dateValue{}, false
	}
	d := dateOf(dateRe.FindStringSubmatch(v))
	return d, len(d.days) > 0 || d.order != ""
}

// dateSet is the dates a document writes.
type dateSet struct {
	days   map[string]bool
	orders map[string]bool
}

// datesIn is every date written in text. A bill writes "04-Oct-2026" and ERPNext's date field takes 04-10-2026:
// found live, the plain text match refused the bill's own date and the operator asked a person to approve typing it.
func datesIn(text string) *dateSet {
	set := &dateSet{days: map[string]bool{}, orders: map[string]bool{}}
	for _, m := range dateRe.FindAllStringSubmatch(text, -1) {
		d := dateOf(m)
		for _, day := range d.days {
			set.days[day] = true
		}
		if d.order != "" {
			set.orders[d.order] = true
		}
	}
	return set
}

// holds says whether the document writes the date v means. A typed date that is ambiguous is read both ways round
// against the days the document names outright (a form that wants month-first is fed the same bill), but against a
// document date that is itself ambiguous only the same field order counts: 10-04-2026 is not 04-10-2026 written
// another way, and a swapped day and month is the quiet error this check exists to stop. The converse - an
// unambiguous value (2026-10-04) from an ambiguous document date - is not traced either; the operator asks.
func (s *dateSet) holds(v dateValue) bool {
	for _, day := range v.days {
		if s.days[day] {
			return true
		}
	}
	if v.order == "" {
		return false
	}
	if s.orders[v.order] {
		return true
	}
	var a, b, y int
	if _, err := fmt.Sscanf(v.order, "%d/%d/%d", &a, &b, &y); err != nil {
		return false
	}
	return s.days[fmt.Sprintf("%04d-%02d-%02d", y, b, a)] || s.days[fmt.Sprintf("%04d-%02d-%02d", y, a, b)]
}

// normalise lowercases and collapses whitespace, so a value matches however
// a document spaced or capitalised it.
func normalise(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// positional drops --flag and --flag VALUE pairs, leaving the arguments.
func positional(words []string) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if strings.HasPrefix(w, "--") {
			if w != "--append" && !strings.Contains(w, "=") {
				i++ // its value
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

// shellWords splits a command line the way a POSIX shell would for plain
// words and single or double quotes (no expansion).
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	in, quote, escaped := false, rune(0), false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}

// unquote strips one layer of quotes from a --param value.
func unquote(v string) string {
	if w := shellWords(v); len(w) == 1 {
		return w[0]
	}
	return v
}
