package skills

import (
	"fmt"
	"regexp"
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
	documentPathRe = regexp.MustCompile(`(?i)(?:~|/)[\w.~/-]*[\w-]\.(?:pdf|xlsx|xlsm|xls|ods|docx|doc|odt|csv|eml|png|jpe?g|tiff?)\b`)
	// paramRe is one --param NAME=VALUE of gui do or gui replay.
	paramRe = regexp.MustCompile(`--param\s+[\w.-]+=("(?:[^"\\]|\\.)*"|'[^']*'|\S+)`)
)

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
			if item = strings.TrimSpace(strings.ReplaceAll(item, `\n`, "")); item != "" {
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
	for _, doc := range documentPathRe.FindAllString(st.Request, -1) {
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
	var untraced []string
	for _, v := range values {
		if !strings.Contains(seen, normalise(v)) {
			untraced = append(untraced, fmt.Sprintf("%q", v))
		}
	}
	if len(untraced) == 0 {
		return capitalsNudge(st.Seen, values)
	}
	return fmt.Sprintf("Not run: %s appear nowhere in the request or in anything a command has shown you, so they "+
		"would be guesses. Find each value in the document or the page it comes from and use it exactly as written "+
		"there. If you converted one (a date's format, a unit), or it cannot be found, ask with HELP instead.",
		strings.Join(untraced, ", "))
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
	for _, doc := range documentPathRe.FindAllString(st.Request, -1) {
		if !readBefore(st.Commands, doc) {
			return fmt.Sprintf("Not taken: the request names %s and nothing has read it or done anything yet. "+
				"Do the task first (office read %s to start), and answer DONE only with what the commands showed.", doc, doc)
		}
	}
	return ""
}

// readBefore reports whether an earlier command named the document (by its
// full path, or its file name when it was reached by a relative path).
func readBefore(commands []string, doc string) bool {
	base := doc[strings.LastIndex(doc, "/")+1:]
	for _, c := range commands {
		if strings.Contains(c, doc) || strings.Contains(c, base) {
			return true
		}
	}
	return false
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
