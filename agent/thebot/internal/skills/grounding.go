package skills

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"thebot/internal/atomizer"
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

// maxUntracedNudges bounds the refusals before the person is asked, counted since the work last typed what it could
// trace (noteTracedTyping): a worker that found its values after a refusal starts again. Found live (2026-10-25): two
// refusals of other values ("5%", a total), each put right, had used a goal-wide budget of two, so the next value - a
// sum of two bill lines the worker had not printed - went to the person at once, who answered "write that working
// down, then type it": what the refusal itself says. A worker that keeps guessing never resets it.
const maxUntracedNudges = 2

// untracedSendBack reports whether values a command could not trace go back to the operator (else to the person),
// counting the refusal when they do.
func (st *operatorState) untracedSendBack() bool {
	if st.UntracedNudges >= maxUntracedNudges {
		return false
	}
	st.UntracedNudges++
	return true
}

// noteTracedTyping starts the refusals' count again when a command typing values passed the trace.
func (st *operatorState) noteTracedTyping(command string) {
	if len(typedValues(command)) > 0 {
		st.UntracedNudges = 0
	}
}

// maxSeenChars bounds what is kept of the request and command outputs. Long enough for the longest goals measured: a
// goal is told its checks still know what it was shown, and 400,000 was passed by one that compacted eleven times.
// Values are matched in it as whole tokens (holdsToken), but the more text, the likelier a short value turns up by
// chance: 2,000,000 was more than any goal needed, so it is a megabyte now, with the request always kept
// (maxSeenHeadChars).
const maxSeenChars = 1_000_000

// maxSeenHeadChars bounds the part of the seen text that is never dropped: the request and what earlier steps read.
const maxSeenHeadChars = 200_000

var (
	// documentPathRe is a document named by its path in a request.
	// A Windows worker's documents are named C:\...\po.pdf.
	documentPathRe = regexp.MustCompile(`(?i)(?:~|/|\b[a-z]:[\\/])[\w.~/\\-]*[\w-]\.(?:pdf|xlsx|xlsm|xls|ods|docx|doc|odt|csv|eml|png|jpe?g|tiff?)\b`)
	// paramRe is one --param NAME=VALUE of gui do or gui replay.
	paramRe = regexp.MustCompile(`--param\s+[\w.-]+=("(?:[^"\\]|\\.)*"|'[^']*'|\S+)`)
)

// pathInsideWordRe is a character a path cannot follow when it starts there: it would be the middle of a longer one.
var pathInsideWordRe = regexp.MustCompile(`[\w.~-]`)

// attachedFilesMark opens the lines of a request that name the files a
// person attached to it, one path a line (the control plane's
// attachmentsMark, the desktop agent's attachments.MARK).
const attachedFilesMark = "Files attached to this request, saved on this desktop:"

// requestDocuments returns the documents a request names: paths with a
// document's extension anywhere in it, and every file attached to it,
// whatever its kind - a person attaches a file for it to be read.
func requestDocuments(request string) []string {
	var docs []string
	for _, loc := range documentPathRe.FindAllStringIndex(request, -1) {
		if loc[0] > 0 && pathInsideWordRe.MatchString(request[loc[0]-1:loc[0]]) {
			continue // the tail of a relative path, not a path of its own
		}
		if doc := request[loc[0]:loc[1]]; !slices.Contains(docs, doc) {
			docs = append(docs, doc)
		}
	}

	if _, rest, found := strings.Cut(request, attachedFilesMark); found {
		for _, line := range strings.Split(rest, "\n") {
			if line = strings.TrimSpace(line); line != "" && !slices.Contains(docs, line) {
				docs = append(docs, line)
			}
		}
	}

	return docs
}

// partSepRe is an ordinary separator between the parts of one typed label: " - ", " – ", " / ", " | ", ", ", or
// parentheses ("MUG-12 - Ceramic mug 12oz", "Cafe Bloom (Pune)").
var partSepRe = regexp.MustCompile(`\s+[-\x{2013}\x{2014}/|]\s+|,\s+|\s*\(\s*|\s*\)\s*`)

// partsTraced reports whether a typed value is made only of parts that were each seen, whole, joined by ordinary
// separators. Found live (2026-10-20, P2's portal): the product option "MUG-12 - Ceramic mug 12oz" - the code and the
// description, both read many times - was refused as a guess, and the model took the refusal to mean the item did not
// exist. Strict: two parts or more, every part traced as a whole word or phrase, nothing but separators between.
func partsTraced(v, seen string) bool {
	parts := partSepRe.Split(strings.TrimSpace(v), -1)
	n := 0
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n++
		// Whole: bounded by space, a cell or field separator, or a quote - not by "-" or "." ("MUG" is not "MUG-12").
		whole := regexp.MustCompile(`(?:^|[\s|,;:()"'])` + regexp.QuoteMeta(normalise(part)) + `(?:$|[\s|,;:()"'.])`)
		if !whole.MatchString(seen) {
			return false
		}
	}
	return n >= 2
}

// optionLabelRe is an application's option written as "CODE-Description" ("KGS-KILOGRAMS", "29-Karnataka").
// (Its description is words only: "MUG-12 extra Ceramic mug 12oz" is an item's label, not a list's option.)
var optionLabelRe = regexp.MustCompile(`^\s*([A-Za-z0-9.]{1,10})\s*-\s*([A-Za-z][A-Za-z .&'()]{1,59}?)\s*$`)

// optionTraced reports whether an option label's code (when it has a letter) or its whole description is in what
// was read: the source says "KGS" or "Karnataka", the application lists "KGS-KILOGRAMS" and "29-Karnataka". Found live
// (2026-10-21): the unit and state options chosen for invoice lines were refused as never read, and
// the person was asked where "KGS-KILOGRAMS" came from, seven times. A name none of whose parts match still is not.
func optionTraced(v, seen string) bool {
	m := optionLabelRe.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	whole := func(part string) bool {
		return regexp.MustCompile(`(?:^|[\s|,;:()"'=])` + regexp.QuoteMeta(normalise(part)) + `(?:$|[\s|,;:()"'.])`).MatchString(seen)
	}
	return (letterRe.MatchString(m[1]) && whole(m[1])) || whole(m[2])
}

// outputNameRe finds where a request asks for a file to be made: "save it as X.xlsx", "make X.docx", a numbered
// deliverable "4. X.xlsx: ...".
var outputNameRe = regexp.MustCompile(`(?im)(?:\b(?:save[sd]?|saving|as|make|create|write|build|produce|export|into)\s+(?:it\s+|them\s+|the\s+\w+\s+)?(?:as\s+)?|^\s*\d{1,2}[.)]\s*)["'\x{201c}]?(?:[\w./~-]*/)?([\w.-]+\.[A-Za-z0-9]{2,5})\b`)

// fillerWordRe is a value that only fills a field: test, dummy, n/a, tbd, xxx ...
var fillerWordRe = regexp.MustCompile(`(?i)^\W*(?:test(?:ing)?|dummy|sample|placeholder|x{3,}|asdf\w*|qwerty|lorem(?: ipsum.*)?|n\s*/?\s*a|tbd|to be (?:confirmed|decided)|none|null|unknown)\W*$`)

// fillerDomainRe is an email address at a domain that stands for no one.
var fillerDomainRe = regexp.MustCompile(`(?i)@(?:example|test|sample|dummy|domain|email|mail)\.(?:com|org|net|in|co\.in)\b`)

var nonDigitRe = regexp.MustCompile(`\D`)

// looksFiller reports whether a typed value only fills a field: a filler word, an address at example.com, or a number
// of five or more digits that is one digit repeated or a plain run (1234567890, 9876543210). (The same rule as
// decurix_agent/filler.py.)
func looksFiller(v string) bool {
	t := strings.TrimSpace(v)
	if t == "" {
		return false
	}
	if fillerWordRe.MatchString(t) || fillerDomainRe.MatchString(t) {
		return true
	}
	digits := nonDigitRe.ReplaceAllString(t, "")
	bare := regexp.MustCompile(`[\s+\-().]`).ReplaceAllString(t, "")
	if len(digits) < 5 || len(digits) < len(bare) {
		return false
	}
	if strings.Count(digits, digits[:1]) == len(digits) {
		return true
	}
	return strings.Contains("01234567890123456789", digits) || strings.Contains("98765432109876543210", digits)
}

// inSeen reports whether a value was shown: as a whole token (holdsToken), or - a long number such as a phone - by
// its digits, grouped as the text grouped them ("98450 12345" is 9845012345).
func inSeen(seen, v string) bool {
	if holdsToken(normalise(seen), normalise(v)) {
		return true
	}
	if n, ok := canonicalNumber(v); ok && numbersIn(seen)[n] {
		return true // the same figure written another way: 2750 for 2,750.00
	}
	digits := nonDigitRe.ReplaceAllString(v, "")
	return len(digits) >= 5 && digitGroupsHold(seen, digits)
}

// isWordRune is a rune a token is made of: a letter or a digit.
func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// holdsToken reports whether text holds v as a whole token, both already normalised: where v starts (or ends) with a
// letter or digit, the text next to it is no letter or digit, and where v starts (or ends) with a digit it is not
// inside a longer figure either ("50" is not in "1,500" or "2.50", "1,500" is not in "1,500.25"). A plain substring
// test made "50" seen because "1,500" was, across up to megabytes of output. (Figures written differently - 1,500
// and 1500 - are matched by their canonical form, numbersIn, not here.)
func holdsToken(text, v string) bool {
	if v == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(v)
	last, _ := utf8.DecodeLastRuneInString(v)
	for from := 0; from <= len(text)-len(v); {
		i := strings.Index(text[from:], v)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(v)
		if tokenStartsAt(text, start, first) && tokenEndsAt(text, end, last) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		from = start + size
	}
	return false
}

// tokenStartsAt reports whether a token starting with rune first may start at text[start:].
func tokenStartsAt(text string, start int, first rune) bool {
	if start == 0 || !isWordRune(first) {
		return true
	}
	prev, size := utf8.DecodeLastRuneInString(text[:start])
	if isWordRune(prev) {
		return false
	}
	if unicode.IsDigit(first) && (prev == '.' || prev == ',') {
		before, _ := utf8.DecodeLastRuneInString(text[:start-size])
		return !unicode.IsDigit(before) // "1,500" holds no "500"
	}
	return true
}

// tokenEndsAt reports whether a token ending with rune last may end at text[:end].
func tokenEndsAt(text string, end int, last rune) bool {
	if end == len(text) || !isWordRune(last) {
		return true
	}
	next, size := utf8.DecodeRuneInString(text[end:])
	if isWordRune(next) {
		return false
	}
	if unicode.IsDigit(last) && (next == '.' || next == ',') {
		after, _ := utf8.DecodeRuneInString(text[end+size:])
		return !unicode.IsDigit(after) // "1,500.25" holds no "1,500"
	}
	return true
}

// digitGroupsRunRe is a run of digit groups joined as a phone or an account number is written: by single spaces, dashes,
// dots or brackets ("+91 98450-12345", "(080) 2345 6789").
var digitGroupsRunRe = regexp.MustCompile(`\d+(?:[ ().-]{1,2}\d+)*`)

// digitGroupsHold reports whether text holds digits as consecutive whole groups of one run of digit groups: the
// digits of "+91 98450 12345" hold 9845012345 and 919845012345, never 4501 or digits joined across two unrelated
// figures. (Before, every digit of the whole seen text was run together and searched, so almost any long number
// was "seen".)
func digitGroupsHold(text, digits string) bool {
	for _, run := range digitGroupsRunRe.FindAllString(text, -1) {
		groups := digitGroupRe.FindAllString(run, -1)
		for i := range groups {
			joined := ""
			for _, g := range groups[i:] {
				joined += g
				if len(joined) >= len(digits) {
					break
				}
			}
			if joined == digits {
				return true
			}
		}
	}
	return false
}

// digitGroupRe is one group of digits.
var digitGroupRe = regexp.MustCompile(`\d+`)

// unreadSources are the documents the request names or brings that are sources still to read (one path a file name):
// a file the request asks to be made is not a source; a file that is not there (yet) cannot be
// read; and reading any file of the same name - the same file in another folder the request ties to it - reads it.
func unreadSources(st *operatorState) []string {
	outputs := map[string]bool{}
	for _, m := range outputNameRe.FindAllStringSubmatch(st.Request, -1) {
		outputs[strings.ToLower(m[1])] = true
	}
	var names []string
	seen := map[string]bool{}
	for _, doc := range requestDocuments(st.Request) {
		base := docName(doc)
		key := strings.ToLower(base)
		if base == "" || seen[key] || outputs[key] {
			continue
		}
		seen[key] = true
		if sourceMissing(expandHome(doc)) {
			continue
		}
		if !readBefore(st.Commands, doc) && !readElsewhere(st.Commands, expandHome(doc)) {
			names = append(names, doc)
		}
	}
	return names
}

// commandPathRe is a path-like word of a command: a folder it may have read or listed.
var commandPathRe = regexp.MustCompile(`[\w.~/-]*[\w-]`)

// readElsewhere reports whether a command read the same file in another folder: a folder a command names holds a file
// of that name with the same content. Found live (2026-10-20, P2): the request said the attached files are also in a
// folder of the work folder; the worker read those copies and the guard demanded the attached ones.
func readElsewhere(commands []string, doc string) bool {
	base := docName(doc)
	want, err := os.ReadFile(doc)
	if err != nil || base == "" {
		return false
	}
	checked := map[string]bool{}
	for _, c := range commands {
		for _, word := range commandPathRe.FindAllString(c, -1) {
			word = expandHome(strings.TrimRight(word, "/"))
			// The word itself or its folder; a relative one from the work folder, where commands run.
			for _, dir := range []string{word, filepath.Dir(word), filepath.Join(workFolder(), word), filepath.Join(workFolder(), filepath.Dir(word))} {
				if checked[dir] {
					continue
				}
				checked[dir] = true
				if info, err := os.Stat(dir); err != nil || !info.IsDir() {
					continue
				}
				other := filepath.Join(dir, base)
				if filepath.Clean(other) == filepath.Clean(doc) {
					continue
				}
				if got, err := os.ReadFile(other); err == nil && bytes.Equal(got, want) {
					return true
				}
			}
		}
	}
	return false
}

// workFolder is where the operator's commands run (the agent's work folder).
func workFolder() string {
	if w := os.Getenv("DECURIX_WORK_DIR"); w != "" {
		return w
	}
	return expandHome("~/work")
}

// sourceMissing reports whether a file is not there to be read (a variable so tests can name files that do not exist).
var sourceMissing = func(path string) bool {
	_, err := os.Stat(path)
	return err != nil
}

// expandHome is a path with a leading ~ made the home folder.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// unreadNudge sends the operator to read the sources it has not: named by their plain names (what the person's
// question repeats), with the path to read.
func unreadNudge(docs []string) string {
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		names = append(names, docName(doc))
	}
	return fmt.Sprintf("Not run: a value you would type is not in anything you have read, and the request also gives "+
		"%s; you have not read it. Every value you type must come from what the task gives. Read it first (office "+
		"read %s), then type what it says.", strings.Join(names, ", "), docs[0])
}

// valueTokenRe is a token that can be a value: a run of letters, digits and . , / - _ holding at least one digit.
var valueTokenRe = regexp.MustCompile(`[\p{L}\p{N}._/,-]*\p{N}[\p{L}\p{N}._/,-]*`)

// fileNameTokenRe is a token that names a file: it ends in a document's extension (a path's last part included).
var fileNameTokenRe = regexp.MustCompile(`(?i)\.(?:pdf|docx?|odt|rtf|txt|eml|xlsx|xlsm|xls|ods|csv|png|jpe?g|tiff?|webp|heic|pptx?)['")]*$`)

// withoutCommandValues is a command's output with every value that already stood in the command itself left out: what
// a command prints back of what it was given is not a source of it. Found live (2026-10-23): late in a
// long goal the worker wrote a list of records from memory into a script - two codes invented, record numbers shifted
// by one - the script printed them back, and that printout made them "seen", so typing them into a form passed as
// traced. A value the command computed (not written in it) stays: that is the working.
func withoutCommandValues(output, command string) string {
	given := map[string]bool{}
	for _, t := range valueTokenRe.FindAllString(command, -1) {
		if fileNameTokenRe.MatchString(t) {
			// A file's name is a name, not a value the command vouches for: masked, the header a read opens with
			// ("SSS-26-27-0199.pdf:") matched no offered document and the bill was "never read" (found live,
			// 2026-10-26, four runs over).
			continue
		}
		given[strings.Trim(t, ".,/-_")] = true
	}
	if len(given) == 0 {
		return output
	}
	return valueTokenRe.ReplaceAllStringFunc(output, func(t string) string {
		if given[strings.Trim(t, ".,/-_")] {
			return "·"
		}
		return t
	})
}

// noteSeen adds text the operator has now seen (the request, a command's output, a person's reply), keeping at most
// maxSeenChars. The first text noted (the request and what earlier steps read, up to maxSeenHeadChars) is always kept; the rest is
// the most recent output, cut at a line so no token is cut in half.
func (st *operatorState) noteSeen(text string) {
	if st.Seen == "" && st.SeenHead == 0 {
		head := "\n" + text
		if len(head) > maxSeenHeadChars {
			head = trimRuneEnd(head[:maxSeenHeadChars])
		}
		st.SeenHead = len(head)
	}
	st.Seen += "\n" + text
	if len(st.Seen) <= maxSeenChars {
		return
	}
	head := st.Seen[:min(st.SeenHead, len(st.Seen))]
	tail := st.Seen[len(st.Seen)-(maxSeenChars-len(head)):]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl < 4096 {
		tail = tail[nl:]
	} else {
		tail = trimRuneStart(tail) // no line break near the cut: at least do not keep half a rune
	}
	st.Seen = head + tail
}

// trimRuneStart drops any bytes at the start of s that are the tail of a multi-byte rune cut mid-character, so s
// begins on a valid UTF-8 boundary. A byte-position cut (noteSeen) can land inside a rune; the model and the
// grounding check are both fed this text, and invalid UTF-8 at the edge breaks a quote match and an API call alike.
func trimRuneStart(s string) string {
	for i := 0; i < len(s) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(s[i]) {
			return s[i:]
		}
	}
	return s
}

// trimRuneEnd drops any bytes at the end of s that are an incomplete multi-byte rune, so s ends on a whole rune.
func trimRuneEnd(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// shellSegment is one simple command of a command line - the words between ;, &&, ||, | and line ends - with the
// body of a here-document it reads on stdin, when it has one.
type shellSegment struct {
	Text     string
	Stdin    string
	HasStdin bool
}

// heredocRe opens a here-document: `<<'EOF'`, <<EOF, <<-"END".
var heredocRe = regexp.MustCompile(`<<(-?)[ \t]*(?:'(\w+)'|"(\w+)"|(\w+))`)

// shellSegments splits a command line into its simple commands, as the shell runs them: separators inside quotes
// are text, and a here-document's lines are the stdin of the command that opened it, not commands of their own.
// Found live (2026-10-19): `gui type e12 "D1" && gui key enter && ... && gui key ctrl+d` was read as one command
// typing "ctrl+d" (the chain's last word), and refused as a value nobody had given.
func shellSegments(command string) []shellSegment {
	var segs []shellSegment
	var cur strings.Builder
	type pending struct {
		seg   int
		word  string
		strip bool
	}
	var waiting []pending
	flush := func() {
		text := strings.TrimSpace(cur.String())
		cur.Reset()
		if text == "" {
			return
		}
		segs = append(segs, shellSegment{Text: text})
		if m := heredocRe.FindStringSubmatchIndex(text); m != nil {
			word := ""
			for g := 2; g <= 4; g++ {
				if m[2*g] >= 0 {
					word = text[m[2*g]:m[2*g+1]]
				}
			}
			segs[len(segs)-1].Text = strings.TrimSpace(text[:m[0]] + text[m[1]:])
			waiting = append(waiting, pending{len(segs) - 1, word, m[3] > m[2]})
		}
	}
	runes := []rune(command)
	quote, escaped := rune(0), false
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			cur.WriteRune(r)
			escaped = true
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == '\n':
			flush()
			// The lines after one that opened here-documents are their bodies, each up to its closing word.
			rest := string(runes[i+1:])
			consumed := 0
			for _, w := range waiting {
				lines := strings.Split(rest[consumed:], "\n")
				var body []string
				found := false
				used := 0
				for _, l := range lines {
					used += len(l) + 1
					closing := l
					if w.strip {
						closing = strings.TrimLeft(l, "\t")
					}
					if strings.TrimRight(closing, " \t\r") == w.word {
						found = true
						break
					}
					body = append(body, l)
				}
				if !found {
					break
				}
				segs[w.seg].Stdin, segs[w.seg].HasStdin = strings.Join(body, "\n"), true
				consumed += used
			}
			waiting = nil
			if consumed > 0 {
				i += len([]rune(rest[:min(consumed, len(rest))]))
			}
		case r == ';' || r == '|' || r == '&':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return segs
}

// typedValues returns the values a command would type into an app: gui do's
// and gui replay's --param values (each item of a "; " list), and the text
// of gui type or the option of gui select - of every gui command in a chain.
// Nil when there are none. A key chord (gui key ctrl+d) is never one.
func typedValues(command string) []string {
	return typedValuesExcept(command, nil)
}

// typedValuesExcept is typedValues leaving out the simple commands skip says to.
func typedValuesExcept(command string, skip func(segment string) bool) []string {
	var out []string
	for _, seg := range shellSegments(command) {
		if skip != nil && skip(seg.Text) {
			continue
		}
		out = append(out, segmentValues(seg)...)
	}
	return out
}

// segmentValues is what one simple gui command would type (see typedValues).
func segmentValues(seg shellSegment) []string {
	words := shellWords(seg.Text)
	if len(words) < 2 || words[0] != "gui" {
		return nil
	}

	var raw []string
	switch words[1] {
	case "do", "replay":
		for _, m := range paramRe.FindAllStringSubmatch(seg.Text, -1) {
			raw = append(raw, unquote(m[1]))
		}
	case "type", "select":
		args := positional(words[2:])
		switch {
		case len(args) == 0:
		case seg.HasStdin && args[len(args)-1] == "-":
			// gui type REF - <<'EOF' ... EOF: the text is the here-document's body, verbatim.
			raw = append(raw, seg.Stdin)
		case len(args) >= 2 && refRe.MatchString(args[0]):
			raw = append(raw, strings.Join(args[1:], " "))
		default:
			raw = append(raw, args[len(args)-1])
		}
	default:
		return nil
	}

	var out []string
	for _, v := range raw {
		// A tab between values moves to the next cell, as a newline moves to the next line: each part is a value of its
		// own. Found live (2026-10-15): a deck's table row typed as "655\tEast\t498" was refused as one value nobody had
		// given. Both a real tab and its written mark (\t) count.
		// Not inside path-looking text: C:\new\temp\notes.txt holds no Enter and no Tab (pathSpans).
		v = outsidePaths(v, func(part string) string {
			return strings.ReplaceAll(strings.ReplaceAll(part, `\t`, "\t"), `\n`, "\n")
		})
		for _, item := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == '\n' || r == '\t' }) {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

// untracedQuestion is what a person is asked when the operator keeps typing values it cannot trace: the values
// themselves, plainly, and what to answer - never the guard's own wording. Found live (2026-10-19): a clerk was asked
// "Which values should I use? ("FALSE" appear nowhere in the request or in anything a command has shown you ...)".
func untracedQuestion(untraced []string, nudge string) string {
	// Found live (2026-10-20, round 5 P2): a planned stage asked "Which document or page should I take it from?" naming
	// nothing - the person could not answer it, and an answer about something else was taken for its answer. Every
	// path that refuses to type now hands over the values; this one is for a refusal that names none.
	if len(untraced) == 0 {
		return "I stopped before typing a value whose source I could not find (" + truncateText(strings.TrimPrefix(nudge, "Not run: "), 300) +
			"). Which document or page should I take it from?"
	}
	if m := unreadNamedRe.FindStringSubmatch(nudge); m != nil {
		return "I was about to type " + quotedList(untraced) + ", but I have not found it in what I have read, and I " +
			"could not read " + m[1] + ", so I have not typed it. Where does it come from - or what should I type instead?"
	}
	return "I was about to type " + quotedList(untraced) + ", but I could not find this in your request or in " +
		"the files and screens I read, so I have not typed it. Where does it come from - or what should I type " +
		"instead?"
}

// secretShown is what a refusal or a question says in place of a secret value: the field, never the value. A password
// in the chat is the leak privacy.py exists to prevent (found live 2026-10-20).
const secretShown = "the password (not shown here)"

// requestSecretRe is a secret the request spells out after its name: "password Northwind#2026", "PIN: 4821".
// (The same rule as decurix_agent/computer/privacy.py's REQUEST_SECRET.)
var requestSecretRe = regexp.MustCompile(`(?i)\b(?:pass(?:word|code|phrase)|pwd|pin|otp|one[- ]time (?:code|password)|token|api[ _-]?key|secret(?: key)?)\b\s*(?:is\s+|[:=]\s*)?["'\x{201c}\x{2018}]?([^\s"'\x{201d}\x{2019},;)\]]{3,200})`)

// passwordFieldRe is a password field in a gui observe listing: its ref.
var passwordFieldRe = regexp.MustCompile(`(?m)^(e\d+) password text\b`)

// secretValues are the typed values of command that are secrets: what the request gives after a password's (PIN's,
// key's) name, and anything typed into a field the screen listed as a password field.
func secretValues(st *operatorState, command string) map[string]bool {
	out := map[string]bool{}
	for _, m := range requestSecretRe.FindAllStringSubmatch(st.Request, -1) {
		out[strings.TrimRight(m[1], ".!?")] = true
	}
	fields := map[string]bool{}
	for _, m := range passwordFieldRe.FindAllStringSubmatch(st.Seen, -1) {
		fields[m[1]] = true
	}
	if len(fields) > 0 {
		for _, seg := range shellSegments(command) {
			words := shellWords(seg.Text)
			if len(words) >= 4 && words[0] == "gui" && words[1] == "type" && fields[words[2]] {
				for _, v := range segmentValues(seg) {
					out[v] = true
				}
			}
		}
	}
	return out
}

// maskSecrets is values with every secret replaced by secretShown.
func maskSecrets(values []string, secrets map[string]bool) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if secrets[v] {
			v = secretShown
		}
		out = append(out, v)
	}
	return out
}

// unreadNamedRe finds the documents a "you have not read it" refusal names.
var unreadNamedRe = regexp.MustCompile(`the request also gives (.+?); you have not read it`)

// quotedList is values as a person reads them: quoted, at most five, the rest counted.
func quotedList(values []string) string {
	shown := make([]string, 0, len(values))
	for i, v := range values {
		if i == 5 {
			shown = append(shown, fmt.Sprintf("and %d more", len(values)-5))
			break
		}
		if v == secretShown {
			shown = append(shown, v)
			continue
		}
		shown = append(shown, fmt.Sprintf("%q", truncateText(v, 80)))
	}
	return strings.Join(shown, ", ")
}

// maxQuoteNudges bounds how often a gui type whose quoting the shell would change is sent back before it is let run.
const maxQuoteNudges = 2

// mixedQuoting reports whether a simple command has a word whose quoting opens or closes in its middle: "a"b, a"b",
// ="x"" - a quote meant as text, which the shell takes as the end or start of quoting and drops.
func mixedQuoting(segment string) bool {
	quote, escaped, inWord, justClosed := rune(0), false, false, false
	for _, r := range segment {
		switch {
		case escaped:
			escaped, inWord, justClosed = false, true, false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote, justClosed = 0, true
			}
		case r == '"' || r == '\'':
			if inWord {
				return true
			}
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			inWord, justClosed = false, false
		default:
			if justClosed {
				return true
			}
			inWord = true
		}
	}
	return quote != 0
}

// shellExpands reports whether a simple command holds a $ the shell would expand - outside single quotes and before a
// letter, an underscore, { or ( - where a spreadsheet's absolute reference ($A$2) was meant. Found live (2026-10-19):
// "=VLOOKUP(B2,Prices!$A$2:$C$13,3,FALSE)" reached real Excel as =VLOOKUP(B2,Prices!:3,3,FALSE).
func shellExpands(segment string) bool {
	runes := []rune(segment)
	quote, escaped := rune(0), false
	for i, r := range runes {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote == '\'':
			if r == '\'' {
				quote = 0
			}
		case r == '"':
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
		case r == '\'' && quote == 0:
			quote = '\''
		case r == '$' && i+1 < len(runes):
			n := runes[i+1]
			// A digit and the special parameters expand too: found live (2026-10-21), the password
			// "Lift$2026" reached the login form as "Lift026" ($2 is a positional parameter) and the worker reported
			// the person's credentials as wrong.
			if unicode.IsLetter(n) || unicode.IsDigit(n) || strings.ContainsRune("_{(@*#?$!-", n) {
				return true
			}
		}
	}
	return false
}

// unbalancedFormula reports whether a formula's text, as the shell hands it over, has an odd number of quotes or
// unmatched brackets - what a formula whose quotes the shell ate looks like.
func unbalancedFormula(v string) bool {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "=") {
		return false
	}
	return strings.Count(v, `"`)%2 == 1 || strings.Count(v, "(") != strings.Count(v, ")")
}

// quotesNudge is what to tell the operator instead of a gui type whose text the shell would hand over other than as
// written, or "". Found live (2026-10-19): gui type "=IF(LOWER(TRIM(D2))=""maharashtra"",E2*9%,0)" reached the sheet
// as =IF(LOWER(TRIM(D2))=maharashtra,E2*9%,0) (every row #NAME?), and gui type "=IF(D2="NOT FOUND","",C2*D2)" as
// =IF(D2=NOT FOUND,,C2*D2) (FALSE in real Excel).
func quotesNudge(command string) string {
	for _, seg := range shellSegments(command) {
		words := shellWords(seg.Text)
		if len(words) < 2 || words[0] != "gui" || words[1] != "type" || seg.HasStdin {
			continue
		}
		if shellExpands(seg.Text) {
			return "Not run: the text holds $ before a letter or a digit ($A$2, $2026), which the shell replaces inside " +
				"\"...\" - the app would get Sheet2!:3 for Sheet2!$A$2:$C$9, Pass026 for Pass$2026. Give the text verbatim on stdin, with the " +
				"end word quoted so nothing in it is expanded:\ngui type REF - <<'EOF'\n=VLOOKUP(B2,Sheet2!$A$2:$C$9,3,FALSE)\nEOF"
		}
		bad := mixedQuoting(seg.Text)
		for _, v := range segmentValues(seg) {
			bad = bad || unbalancedFormula(v)
		}
		if bad {
			return "Not run: a quote inside the text of a gui type ends or starts the shell's quoting, so the app would " +
				"get the text without those quotes (a formula comparing with \"text\" would break). Give any text that " +
				"holds quotes - every formula - verbatim on stdin, one gui type each, the quotes as the app should get " +
				"them and a real Tab between cells:\ngui type REF - <<'EOF'\n=IF(D2=\"text\",\"\",C2*D2)\nEOF"
		}
	}
	return ""
}

// searchFieldRe names a field typed into to find something - a page, a
// record, a menu entry - not to record a value.
var searchFieldRe = regexp.MustCompile(`(?i)^(search|find|filter|look ?up|go to|jump to)\b`)

// typesIntoSearch reports whether command is a gui type into a field the
// operator has been shown as a search box (searchbox, or a field named
// "Search ..."), found by its ref in what was seen. Typing there finds where
// to go; it records nothing. Found live (2026-10-13): to reach a new Payment
// Entry the operator typed "Payment Entry" into the application's search bar,
// and was refused twice as typing "a value that appears nowhere" - then asked
// a person where the words came from.
func typesIntoSearch(st *operatorState, command string) bool {
	words := shellWords(command)
	if len(words) < 2 || words[0] != "gui" || words[1] != "type" {
		return false
	}
	args := positional(words[2:])
	if len(args) < 2 || !refRe.MatchString(args[0]) {
		return false
	}
	ref := regexp.QuoteMeta(args[0])
	// As gui observe lists it (e12 combobox "Search") and as an action's result names it (combobox "Search" (e12)).
	var role, name string
	best := -1
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)(?:^|\s)` + ref + ` ([a-z]+)(?: "([^"]*)")?`),
		regexp.MustCompile(`([a-z]+) "([^"]*)" \(` + ref + `\)`),
	} {
		for _, m := range re.FindAllStringSubmatchIndex(st.Seen, -1) {
			if m[0] > best {
				best, role = m[0], st.Seen[m[2]:m[3]]
				name = ""
				if m[4] >= 0 {
					name = st.Seen[m[4]:m[5]]
				}
			}
		}
	}
	return best >= 0 && (role == "searchbox" || searchFieldRe.MatchString(strings.TrimSpace(name)))
}

// refRe is an element's ref as gui observe gives it.
var refRe = regexp.MustCompile(`^e[0-9]+$`)

// groundingCheck is what to tell the operator instead of running command, or "" when every value it would type can be
// traced, with the values it could not trace, as typed (for a question a person can answer).
func groundingCheck(st *operatorState, command string) (string, []string) {
	// Each simple command of a chain on its own: one typing into a search box excuses only itself.
	values := typedValuesExcept(command, func(segment string) bool { return typesIntoSearch(st, segment) })
	if len(values) == 0 {
		return "", nil
	}

	secrets := secretValues(st, command)
	// The sources not read yet: files the request names or brings that exist, that it does not ask to be made, and
	// that no command has touched by name (a copy of the same file elsewhere counts). Found live (2026-10-20, P2's
	// portal stage): a deliverable still to make and the inbox copies of files read in the work folder were demanded,
	// and a value the chats gave was refused for them.
	unread := unreadSources(st)
	// A filler (9999999999, test, n/a, name@example.com) is refused whatever else was read - pictures included, which
	// otherwise leave the words untraced. Found live (2026-10-20, P4): after looking at ID pictures, a joiner was
	// onboarded with phone 9999999999; the attached tracker had her number.
	var filler []string
	for _, v := range values {
		if looksFiller(v) && !inSeen(st.Seen, v) {
			filler = append(filler, v)
		}
	}
	if len(filler) > 0 {
		return fmt.Sprintf("Not run: %s looks invented - a filler, not a value read from any source. Never type a value "+
				"you did not read from a source or the person: find the real one (the request's input files, what earlier "+
				"steps made) and give it in the format the field accepts - or ask, or leave an optional field empty.", quotedList(maskSecrets(filler, secrets))),
			maskSecrets(filler, secrets)
	}
	if st.ShownPages && len(unread) > 0 {
		// Read from pictures, with a source still unread: the unread source holds the typing back.
		var waiting []string
		requestText := normalise(st.Request)
		for _, v := range values {
			// (A word, not a bare number: "50" is in many requests and may be a document's figure.)
			whole := regexp.MustCompile(`(?:^|[\s(,;:"'])` + regexp.QuoteMeta(normalise(v)) + `(?:$|[\s),;:."'])`)
			if len(strings.TrimSpace(v)) < 3 || !letterRe.MatchString(v) || !whole.MatchString(requestText) {
				waiting = append(waiting, v)
			}
		}
		if len(waiting) == 0 {
			return "", nil
		}
		return unreadNudge(unread), maskSecrets(waiting, secrets)
	}
	seen := normalise(st.Seen)
	numbers := sync.OnceValue(func() map[string]bool { return numbersIn(st.Seen) })
	dates := sync.OnceValue(func() *dateSet { return datesIn(st.Seen) })
	var untraced, bare []string
	authoring := authoringRe.MatchString(st.Request)
	for _, v := range values {
		if holdsToken(seen, normalise(v)) || holdsToken(seen, normalise(withoutNewlineMarks(v))) {
			continue
		}
		// Words of the worker's own in something the request asks it to write (a letter, a reply, a memo) are
		// authored, not copied: found live (2026-10-15), asked to write a reply letter in Word, every sentence of it
		// was refused as "a guess" and the person was asked which values to use. Its figures are still traced.
		if authoring && letterRe.MatchString(v) {
			figures := true
			// A letter's own date ("15 October 2026") is the writer's, not a figure to trace.
			for _, n := range numberRe.FindAllString(proseDateRe.ReplaceAllString(v, " "), -1) {
				c, ok := canonicalNumber(n)
				figures = figures && ok && numbers()[c]
			}
			if figures {
				continue
			}
		}
		if n, ok := canonicalNumber(v); ok && numbers()[n] {
			continue
		}
		if literals, texts, ok := formulaLiterals(v); ok {
			traced := true
			for _, n := range literals {
				traced = traced && numbers()[n]
			}
			for _, t := range texts {
				traced = traced && holdsToken(seen, normalise(t))
			}
			if traced {
				continue
			}
		}
		if d, ok := canonicalDate(v); ok && (dates().holds(d) || dates().boundsAMonth(d)) {
			continue
		}
		if pathTraced(v, seen) || fileTypeTraced(v, seen) || cellPlaceRe.MatchString(strings.TrimSpace(v)) {
			continue
		}
		if partsTraced(v, seen) || optionTraced(v, seen) {
			continue
		}
		if secrets[v] {
			untraced = append(untraced, secretShown)
		} else {
			untraced = append(untraced, fmt.Sprintf("%q", v))
		}
		bare = append(bare, v)
	}
	bare = maskSecrets(bare, secrets)
	if len(untraced) == 0 {
		if nudge := capitalsNudge(st.Seen, values); nudge != "" {
			return nudge, maskSecrets(values, secrets)
		}
		return "", nil
	}
	if st.ShownPages && len(unread) == 0 {
		// Pages came as pictures: a value in no text (their OCR included) may still be read off a picture - typed, but
		// kept as uncertain, for the commit gate's verifier and the reviewer (evidence.go). Before, once any page had
		// come as a picture, nothing typed was traced at all (found live, 2026-10-23).
		st.notePictureOnly(bare)
		return "", nil
	}
	if len(unread) > 0 {
		// A value traced nowhere, with sources still unread: read them first (a value something read already gives
		// was never held back for them - the request's sign-in, a date a chat gave).
		return unreadNudge(unread), bare
	}
	return fmt.Sprintf("Not run: %s appear nowhere in the request or in anything a command has shown you, so they "+
		"would be guesses. This is only about where the text you typed comes from - it says nothing about whether "+
		"the item or record exists. Find each value in the document or the page it comes from and use it exactly as "+
		"written there; for a list or drop-down, type the code alone or pick the app's own option (gui select REF "+
		"\"option\" with the option as gui observe lists it). A date may be typed in the format its field takes. A "+
		"field the request gives no value for is left as the app has it (Enter past it) - nothing has to be typed "+
		"there. A figure you worked out (a total, a sum of quantities, a tax) is traced once a command works it out "+
		"from the source values and prints it - do the arithmetic with python, then type what it printed. If you "+
		"converted one otherwise (a unit), or it cannot be found, ask with HELP instead.",
		strings.Join(untraced, ", ")), bare
}

var (
	// authoringRe is a request that asks for something to be written: a letter, a reply, a memo, a summary.
	// (send/tell/ask/inform/notify/remind/message: "send <person> a message on <chat app> asking him about the
	// differences" - found live, 2026-10-21: every wording of it was refused as untraced and the person
	// was asked for the exact text.)
	authoringRe = regexp.MustCompile(`(?i)\b(?:write|draft|compose|reply|respond|prepare|word|send|tell|ask|inform|notify|remind|message)\b[^.\n]{0,80}?\b(?:letters?|e-?mails?|memos?|messages?|notes?|repl(?:y|ies)|responses?|summar(?:y|ies)|reports?|documents?|descriptions?|posts?|articles?|paragraphs?|captions?|announcements?|minutes|notices?|cover letters?)\b`)
	letterRe    = regexp.MustCompile(`\p{L}`)
	proseDateRe = regexp.MustCompile(`(?i)\b\d{1,2}(?:st|nd|rd|th)?[ ./-]*(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?[ ./,-]*\d{2,4}\b|\b(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.? \d{1,2}(?:st|nd|rd|th)?,? \d{4}\b|\b\d{4}-\d{2}-\d{2}\b|\b\d{1,2}[./-]\d{1,2}[./-]\d{2,4}\b`)
)

// filePathRe is a typed value that is a file's path: folders, then a name with an extension.
var filePathRe = regexp.MustCompile(`^(~|[A-Za-z]:)?[\\/]?(?:[^\\/\n]+[\\/])+[^\\/\n]+\.[A-Za-z0-9]{1,5}$`)

// workFolders are the places a request's "my work folder" means, written as a typed path would start - and the shared
// folder's place on this desktop, where a file the request names under the person's own folder is.
// (Compared in lower case, so written so.)
var workFolders = []string{"/home/headless/work/", "~/work/", strings.ToLower(sharedLinkPath) + "/"}

// sharedLinkPath is where this desktop sees the one folder of the person's computer it shares (the agent links it).
const sharedLinkPath = "/home/headless/PC"

// sharedFolderHost is the folder this desktop shares, as the person writes it on their computer, or "" when it has
// none (the control plane tells the agent, and the agent's environment is soul's).
func sharedFolderHost() string { return strings.TrimSpace(os.Getenv("DECURIX_SHARED_FOLDER")) }

// sharedRule is the one line the operator's contract gains when this desktop shares a folder (see sharedFolderHost).
func sharedRule(host string) string {
	sep := `\`
	if !strings.Contains(host, `\`) {
		sep = "/"
	}
	return "- The person's shared folder, " + host + " on their computer, is also at " + sharedLinkPath + " on this desktop: a file " +
		"they name under it is at the same place there (" + host + sep + "x.xlsx is " + sharedLinkPath + "/x.xlsx). Read it, and " +
		"make your results in it, with your own tools (office, python) - never by typing them into one of their apps a key " +
		"at a time. Their computer's own apps open the same files (the named-app and source-file rules above say how). " +
		"Nothing else of their computer is reachable. " + sharedLinkPath + " is a link: search it as " + sharedLinkPath +
		"/ (with the slash) - find stops at a link it is given.\n"
}

// guiTypeQuotedRe is a gui type (or select) whose text is in double quotes.
var guiTypeQuotedRe = regexp.MustCompile(`(\bgui\s+(?:type|select)\b[^"\n;&|]*?)"((?:[^"\\]|\\.)*)"`)

// literalTyping gives a gui type the text it wrote when the shell would expand a $ in it ("Lift$2026" reaches the
// field as "Lift026": $2 is a positional parameter): the text goes in single quotes instead, where the shell changes
// nothing. Found live (2026-10-21): sent back to use stdin, the model wrote the same command again until
// the nudges ran out, and the client's password reached the login form wrong every time. What the model meant is the
// text it quoted; only the escapes double quotes allow (\" \\ \$ \`) are undone first.
func literalTyping(command string) string {
	return guiTypeQuotedRe.ReplaceAllStringFunc(command, func(m string) string {
		parts := guiTypeQuotedRe.FindStringSubmatch(m)
		if !shellExpands(`"` + parts[2] + `"`) {
			return m
		}
		var b strings.Builder
		for i := 0; i < len(parts[2]); i++ {
			c := parts[2][i]
			if c == '\\' && i+1 < len(parts[2]) && strings.IndexByte("\"\\$`", parts[2][i+1]) >= 0 {
				i++
				c = parts[2][i]
			}
			b.WriteByte(c)
		}
		return parts[1] + "'" + strings.ReplaceAll(b.String(), "'", `'\''`) + "'"
	})
}

// namedPathRe is a path a command names: in quotes (it may hold spaces), or a bare absolute or home path.
var namedPathRe = regexp.MustCompile(`'(~?/[^'\n]+)'|"(~?/[^"\n]+)"|(?:^|[\s=(])(~?/[^\s'"();|&<>]+)`)

// readableKinds are the files a command's reading makes sources of (documents, tables, pictures, text).
var readableKinds = map[string]bool{".pdf": true, ".docx": true, ".doc": true, ".xlsx": true, ".xlsm": true, ".xls": true,
	".csv": true, ".tsv": true, ".txt": true, ".json": true, ".eml": true, ".jpg": true, ".jpeg": true, ".png": true,
	".pptx": true, ".odt": true, ".ods": true, ".md": true, ".html": true, ".htm": true, ".xml": true}

// noteFilesRead adds the files a command names - and the files right inside a folder it names - to the task's list of
// files the work read (~/.decurix/computer/evidence/<task>.files, the agent's evidence.files_read). Found live
// (2026-10-21): the worker read the Word invoices with a python script, typed their figures into the portal, and the
// form's check that each typed fact has a source refused them - only files read with office read were known. Whatever
// read them, a file the work's commands named is one it could have read. Never fails the command.
func noteFilesRead(command string) {
	task := currentTaskID()
	if task == "" {
		return
	}
	home, _ := os.UserHomeDir()
	var found []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] && len(found) < 80 {
			seen[p] = true
			found = append(found, p)
		}
	}
	for _, m := range namedPathRe.FindAllStringSubmatch(command, -1) {
		p := m[1] + m[2] + m[3]
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
		p = strings.TrimRight(p, ".,:;")
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && readableKinds[strings.ToLower(filepath.Ext(p))] {
			add(p)
		} else if info.IsDir() {
			entries, err := os.ReadDir(p)
			if err != nil || len(entries) > 200 {
				continue
			}
			for _, e := range entries {
				if e.Type().IsRegular() && readableKinds[strings.ToLower(filepath.Ext(e.Name()))] {
					add(filepath.Join(p, e.Name()))
				}
			}
		}
	}
	if len(found) == 0 {
		return
	}
	dir := filepath.Join(decurixDir(home), "computer", "evidence")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, task+".files"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strings.Join(found, "\n") + "\n")
}

// maxNotedOutput bounds how much of one command's output is kept as the task's recorded working.
const maxNotedOutput = 6000

// noteCommandOutput keeps what a command printed as the task's recorded working
// (~/.decurix/computer/evidence/<task>.outputs, one JSON line each; the agent's long_task._page_notes reads it for the
// check that a typed or written fact has a source). Found live (2026-10-21): the category totals a
// script summed from the invoices and printed were refused at the final check as "held by no source" - the agent saw
// files and screens, never what the work's commands printed. A figure only the model's head holds stays unsourced.
func noteCommandOutput(command, output string) {
	task := currentTaskID()
	output = strings.TrimSpace(output)
	if task == "" || output == "" || guiCommandRe.MatchString(command) {
		return // (what gui shows is noted by the gui command itself)
	}
	if len(output) > maxNotedOutput {
		output = output[:maxNotedOutput]
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(decurixDir(home), "computer", "evidence")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	line, err := json.Marshal(map[string]any{"at": time.Now().Unix(), "text": output})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, task+".outputs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// maxSourcesRead bounds how many named files one refusal reads for the trace.
const maxSourcesRead = 16

// notedFilesRead are the files the task's list holds (noteFilesRead, and office read's own notes), oldest first.
func notedFilesRead() []string {
	task := currentTaskID()
	if task == "" {
		return nil
	}
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(decurixDir(home), "computer", "evidence", task+".files"))
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

// readNamedSources reads, with `office read`, the files the work's commands named that the operator has not read for
// the trace yet, and adds what they say to what was seen. True when it read any. Found live (2026-10-21):
// the item codes, units and amounts of Word invoices a script had read were refused as never read, and the worker
// asked the person where 9987 came from - it is printed on the invoice.
func (o *OperatorSkill) readNamedSources(rc RunContext, st *operatorState) bool {
	if st.SourcesRead == nil {
		st.SourcesRead = map[string]bool{}
	}
	read := 0
	for _, path := range notedFilesRead() {
		if st.SourcesRead[path] || read >= maxSourcesRead {
			continue
		}
		st.SourcesRead[path] = true
		res := runShell(rc.Ctx, rc.Dir(), "office read '"+strings.ReplaceAll(path, "'", `'\''`)+"'", 90*time.Second)
		if res.ExitCode == 0 && strings.TrimSpace(res.Output) != "" {
			st.noteSeen(res.Output)
			read++
		}
	}
	if read > 0 {
		rc.Logf("operator", "read %d file(s) the work named, to trace what it would type", read)
	}
	return read > 0
}

// currentTaskID is the agent's current task (task_context: DECURIX_TASK_FILE, default /tmp/decurix-current-task).
func currentTaskID() string {
	path := os.Getenv("DECURIX_TASK_FILE")
	if path == "" {
		path = "/tmp/decurix-current-task"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if id == "" || strings.ContainsAny(id, "/\\. \n") {
		return ""
	}
	return id
}

// decurixDir is the agent's state folder (config.DECURIX_DIR: DECURIX_DIR, default ~/.decurix).
func decurixDir(home string) string {
	if d := os.Getenv("DECURIX_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".decurix")
}

// findRe is the start of a find command, with the options that come before its starting points.
var findRe = regexp.MustCompile(`(^|[;&|(]|\s)find((?:\s+-[HLPO]\w*)*)\s`)

// sharedLinkForms are the ways a command names the shared folder's link itself.
var sharedLinkForms = map[string]bool{sharedLinkPath: true, "~/PC": true, "$HOME/PC": true, "${HOME}/PC": true}

// intoSharedLink makes a find that starts at the shared folder's link start inside it ("PC" -> "PC/"), which is
// what was meant: the link is how this desktop reaches the folder (shared.ensure_link), and find does not follow a
// link it is given as a starting point - it printed the link alone and nothing in it. Found live
// (2026-10-21): `find /home/headless/PC -iname '*Kavya*'` found nothing, the engine took the person's folder for empty
// and worked on a stale copy in its own work folder. A find told to follow links (-L, -H) is left as it is.
func intoSharedLink(command string) string {
	locs := findRe.FindAllStringSubmatchIndex(command, -1)
	if locs == nil {
		return command
	}
	var b strings.Builder
	last := 0
	for _, m := range locs {
		opts := command[m[4]:m[5]]
		if strings.Contains(opts, "-L") || strings.Contains(opts, "-H") {
			continue
		}
		i := m[1]
		for i < len(command) {
			for i < len(command) && (command[i] == ' ' || command[i] == '\t') {
				i++
			}
			if i >= len(command) {
				break
			}
			c := command[i]
			if c == '-' || c == '(' || c == ')' || c == '!' || c == '\\' || c == ';' || c == '|' || c == '&' || c == '>' ||
				c == '<' || c == '\n' {
				break
			}
			start, end := i, i
			inner := ""
			if c == '"' || c == '\'' {
				close := strings.IndexByte(command[i+1:], c)
				if close < 0 {
					break
				}
				end = i + 1 + close + 1
				inner = command[i+1 : end-1]
			} else {
				for end < len(command) && !strings.ContainsRune(" \t\n;|&><)", rune(command[end])) {
					end++
				}
				inner = command[start:end]
			}
			if sharedLinkForms[inner] {
				at := end
				if c == '"' || c == '\'' {
					at = end - 1
				}
				b.WriteString(command[last:at])
				b.WriteString("/")
				last = at
			}
			if end <= i {
				// No word was taken: never loop on the same place. Found live (2026-10-25): "find )" in a script's
				// text left end at i, and the soul spun for half an hour with the task stopped.
				break
			}
			i = end
		}
	}
	b.WriteString(command[last:])
	return b.String()
}

// windowTitleRe is a window's title as gui commands report it: `Opened "Document1 - App"`, `in "App - Chromium"`,
// `active window is now "..."`, `window "..." opened`, `Active window: "..."`.
var windowTitleRe = regexp.MustCompile(`(?:Opened|\bin|active window is now|Active window:|window)\s+"([^"\n]{1,160})"`)

// namedAppWindow reports whether a gui command that worked showed a window of an app the request names to work in: a
// window whose title has that app's name as a word. Another app opened in its place (a different word processor for
// the one named) is not it. Found live (2026-10-19): asked to write letters "in Word (the Word app on your desktop)" on a
// desktop with no such app, the worker opened another word processor, and the check of "the app was opened" took any
// gui command for it.
func namedAppWindow(request, command, output string) bool {
	if !guiCommandRe.MatchString(command) {
		return false
	}
	apps := atomizer.AppsFor(request)
	if len(apps) == 0 {
		return false
	}
	for _, m := range windowTitleRe.FindAllStringSubmatch(output, -1) {
		for _, app := range apps {
			if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(app) + `\b`).MatchString(m[1]) {
				return true
			}
		}
	}
	return false
}

// guiOpenRe is a gui open command (the app or file it names follows).
var guiOpenRe = regexp.MustCompile(`(?i)^\s*gui\s+open\b`)

// appOpenAttempts says, in one or two sentences, what was done to open `app`: each gui open that named it with what it
// answered, or else what was opened instead - never a guess. Found live (round 5 P4, 2026-10-20): "I could not open
// word here (it may not be one of this desktop's apps)" on a desktop that had Word (Host); the worker had never tried
// it - it had opened the .docx with gui open FILE, which takes office files to LibreOffice.
func appOpenAttempts(st *operatorState, app string) string {
	nameRe := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(app) + `\b`)
	var named, other []string
	for _, o := range st.Opens {
		command := o
		if i := strings.Index(o, " -> "); i >= 0 {
			command = o[:i]
		}
		if nameRe.MatchString(command) {
			named = append(named, o)
		} else {
			other = append(other, o)
		}
	}
	if len(named) > 0 {
		if len(named) > 3 {
			named = named[len(named)-3:]
		}
		return "I tried to open " + app + " and it did not open: " + strings.Join(named, "; ") + "."
	}
	if len(other) > 0 {
		return "I did not open " + app + " itself - I opened " + other[len(other)-1] + " instead, and that is not " + app + "."
	}
	return "I did not open " + app + " at all."
}

// sharedAppUntouchedNudge is what to tell the operator instead of taking a DONE, on a desktop with a shared folder, for
// a request that names an app to work in when no window of that app was ever opened - the result made with its own
// tools, or in another app. "" when nothing is wrong (or the request names no app, or there is no shared folder).
func sharedAppUntouchedNudge(st *operatorState) string {
	host := sharedFolderHost()
	apps := atomizer.AppsFor(st.Request)
	if len(apps) == 0 || st.NamedAppOpened {
		return ""
	}
	worked := false
	for _, c := range st.Worked {
		worked = worked || guiCommandRe.MatchString(c) || madeBehindRe.MatchString(c)
	}
	if !worked {
		return ""
	}
	app := apps[0]
	where := "open the file in it (gui open FILE)"
	if host != "" {
		where = "open the file in it by its place on their computer (" + host + " and the file's name)"
	}
	return "Not taken: the request asks for this to be done in " + app + ", and no window of " + app + " was opened - " +
		"the result was made with your own tools, or in another app. Open " + app + " (gui open with its name as this " +
		"desktop lists it, an app of the person's computer included), " + where + ", look at it there, do whatever only " +
		"that app does, check it, and save it from the app (Save, keeping its name and format). If " + app + " is not on " +
		"this desktop, do not use another app in its place: reply HELP saying so.\n\nReply now with exactly:\nCOMMAND:\ngui observe"
}

// madeWithTools reports whether one of the commands that worked made a file with tools (office create/update, a
// script that saves one).
func madeWithTools(st *operatorState) bool {
	for _, c := range st.Worked {
		if madeBehindRe.MatchString(c) {
			return true
		}
	}
	return false
}

// savedInAppRe is a gui command that saves from the app in front: its keys (Ctrl+S, F12 for Save As) or a click on
// its Save / Save As control, as gui reports it.
var (
	savedInAppKeyRe = regexp.MustCompile(`(?i)^\s*gui\s+key\s+(?:ctrl\+s|ctrl\+shift\+s|f12)\b`)
	savedInAppOutRe = regexp.MustCompile(`(?i)\bclick(?:ed)?\s+[\w ]{0,20}"Save(?: As)?"`)
)

// savedInApp reports whether a gui command that worked saved from the app in front.
func savedInApp(command, output string) bool {
	return guiCommandRe.MatchString(command) && (savedInAppKeyRe.MatchString(command) || savedInAppOutRe.MatchString(output))
}

// sharedAppUnsavedNudge is what to tell the operator instead of taking a DONE when, on a desktop with a shared
// folder, the content was made with tools in the folder and the named app opened, but nothing was saved from the app:
// the app has to have handled the file. "" when nothing is wrong.
func sharedAppUnsavedNudge(st *operatorState) string {
	host := sharedFolderHost()
	apps := atomizer.AppsFor(st.Request)
	if len(apps) == 0 || !st.NamedAppOpened || st.SavedInApp || !madeWithTools(st) {
		return ""
	}
	app := apps[0]
	place := "the file"
	if host != "" {
		place = "by its place on the person's computer, " + host + " and the file's name"
	}
	return "Not taken yet: the file was made with your tools and " + app + " was opened, but nothing was saved from " +
		app + ". With the file open there (" + place + "), do " +
		"what only " + app + " does if the request asks for it, check it on the screen, and save it from " + app +
		" (Save, keeping its name and format). Then report.\n\nReply now with exactly:\nCOMMAND:\ngui observe"
}

// pathTraced reports whether a typed file path is one the task gave: its file name is in the request or what was
// read, and its folder is the work folder (or below it) or a folder that was itself seen. Found live (2026-10-15):
// asked to save "q3_review.pptx in my work folder", a worker typing /home/headless/work/q3_review.pptx into Impress's
// Save dialog was refused as typing a guess, and asked the person to save the file.
func pathTraced(v, seen string) bool {
	v = strings.TrimSpace(v)
	if !filePathRe.MatchString(v) {
		return false
	}
	cut := strings.LastIndexAny(v, `\/`)
	dir, name := v[:cut+1], v[cut+1:]
	if !holdsToken(seen, normalise(name)) {
		return false
	}
	for _, w := range workFolders {
		if strings.HasPrefix(strings.ToLower(dir), w) {
			return true
		}
	}
	return strings.Contains(seen, normalise(strings.TrimRight(dir, `\/`)))
}

// cellPlaceRe is a spreadsheet cell or range typed to go there (Go To, the Name Box): a place, not a value. Found live
// (2026-10-15): "A20" typed into real Excel's Go To was refused as a guess, and the worker asked the person.
// A sheet's name before it ("Prices!$A:$B", "'Q3 data'!B2") and whole columns or rows ("$A:$B", "2:13") are places too:
// found live (2026-10-19), "Prices!$A:$B" typed into a function's argument box was refused as a value never read.
var cellPlaceRe = regexp.MustCompile(`^(?:(?:'[^']+'|[A-Za-z_][\w.]*)!)?(?:\$?[A-Za-z]{1,3}\$?[0-9]{1,7}(?::\$?[A-Za-z]{1,3}\$?[0-9]{1,7})?|\$?[A-Za-z]{1,3}:\$?[A-Za-z]{1,3}|\$?[0-9]{1,7}:\$?[0-9]{1,7})$`)

// functionLedRe is text that starts with a spreadsheet function call - VLOOKUP(, IFERROR(, SUM( - a formula without
// its "=".
var functionLedRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]*\(.*\)$`)

// fileTypeExtRe finds the extensions a file-type entry names: "Excel Workbook (*.xlsx)", "PDF (*.pdf)".
var fileTypeExtRe = regexp.MustCompile(`\*\.([A-Za-z0-9]{1,5})\b`)

// fileTypeTraced reports whether a value is a Save dialog's file type whose extension the request or what was read
// names (a file to save as q3_review.xlsx asks for "Excel Workbook (*.xlsx)"). Found live (2026-10-15): choosing that
// type in Save As was refused as a guess, and the worker asked the person which values to use.
func fileTypeTraced(v, seen string) bool {
	for _, m := range fileTypeExtRe.FindAllStringSubmatch(v, -1) {
		if strings.Contains(seen, "."+strings.ToLower(m[1])) {
			return true
		}
	}
	return false
}

var (
	cellRefRe        = regexp.MustCompile(`\$?[A-Za-z]{1,3}\$?[0-9]{1,7}`)
	formulaNumberRe  = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
	formulaQuotedRe  = regexp.MustCompile(`"[^"]*"`)
	formulaAllowedRe = regexp.MustCompile(`^=[A-Za-z0-9_.$:;,()+\-*/^%<>=&! 	]+$`)
)

// formulaLiterals reads a spreadsheet formula (=SUM(B2:B5)): the worker's own
// construction, not a value copied from somewhere, so it is not looked for in
// what was read. Found by the GUI benchmark: asked to total B2 to B5 with a
// formula, the operator was refused =SUM(B2:B5) as a value "that appears
// nowhere", and went on to close the spreadsheet. What a formula may not
// carry is a figure nobody has seen: its literal numbers (not cell
// references) are returned to be traced like any typed number, and a formula
// holding quoted text has that text traced like any typed text (texts). Found live (2026-10-19): asked for "Tax A 9% for
// a customer in Region X", =IF(TRIM(D2)="region x",E2*9%,0) was refused as a value "never read" - every quoted
// word made the whole formula a guess - and the worker was pushed off formulas onto typing figures. 0 and 1 are the
// formula's own structure (no result, one whole), never a figure taken from a document.
func formulaLiterals(v string) (numbers, texts []string, ok bool) {
	v = strings.TrimSpace(withoutNewlineMarks(v))
	// A formula whose "=" went separately (gui key "=", then the rest typed) is still a formula: found live
	// (2026-10-19), IFERROR(VLOOKUP(B2,Prices!$A:$B,2,FALSE),"NOT FOUND") was refused as a value never read, and the
	// person was asked where "FALSE" came from.
	if !strings.HasPrefix(v, "=") && functionLedRe.MatchString(v) {
		v = "=" + v
	}
	// "=@IFERROR(...)" is how a spreadsheet shows a formula back (implicit intersection): the same formula.
	if strings.HasPrefix(v, "=@") {
		v = "=" + v[2:]
	}
	if !strings.HasPrefix(v, "=") || len(v) < 2 {
		return nil, nil, false
	}
	for _, q := range formulaQuotedRe.FindAllString(v, -1) {
		if t := strings.TrimSpace(q[1 : len(q)-1]); t != "" {
			texts = append(texts, t)
		}
	}
	bare := formulaQuotedRe.ReplaceAllString(v, `0`)
	if !formulaAllowedRe.MatchString(bare) {
		return nil, nil, false
	}
	for _, n := range formulaNumberRe.FindAllString(cellRefRe.ReplaceAllString(bare, " "), -1) {
		if c, ok := canonicalNumber(n); ok && c != "0" && c != "1" {
			numbers = append(numbers, c)
		}
	}
	return numbers, texts, true
}

// withoutNewlineMarks is a typed value without the \n that gui type reads as
// "press Enter" - unless the value is a Windows path, where \n is the start
// of a folder's name.
func withoutNewlineMarks(v string) string {
	return strings.TrimSpace(outsidePaths(v, func(part string) string { return strings.ReplaceAll(part, `\n`, "") }))
}

var (
	// pathStartRe is where a Windows path begins: a drive (C:\), a share (\\host\) or an environment variable (%VAR%\).
	pathStartRe = regexp.MustCompile(`[A-Za-z]:\\|\\\\[\w.$-]+\\|%[A-Za-z_][A-Za-z0-9_]*%\\`)
	// pathFileRe ends a path as a file: after one of these, \n or \t is an escape again ("C:\a\b.csv\nTotal").
	pathFileRe = regexp.MustCompile(`(?i)\.(?:xlsx|xlsm|xls|ods|csv|tsv|txt|md|log|json|xml|html?|docx?|odt|rtf|pptx?|odp|pdf|png|jpe?g|gif|bmp|tiff?|webp|svg|zip|eml|msg|py|sh|ini|cfg|bak|tmp)$`)
)

// notInAName is what cannot be in a Windows file name, and so ends a stretch of path.
const notInAName = "\\/:*?\"<>|\r\n\t"

// pathSpans is where path-looking text stands in s, as [start, end) byte pairs: from a drive, share or %VAR% to the
// end of its last folder or file - spaces inside names included - or, when a file name (a known extension) is followed
// by a \n or \t escape, to the end of that file. Backslashes inside are literal. The Python side's cli.path_spans is
// the same rule. Found live (2026-10-19): "C:\Users\me\Documents\Decurix demo\receivables.xlsx" was cut at its space
// and the rest, starting \r, was read as an escape.
func pathSpans(s string) [][2]int {
	var spans [][2]int
	pos := 0
	for pos < len(s) {
		loc := pathStartRe.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		begin, i := pos+loc[0], pos+loc[1]
		if begin > 0 && isASCIIAlnum(s[begin-1]) && !(begin >= 2 && (s[begin-2:begin] == `\n` || s[begin-2:begin] == `\t`)) {
			pos = begin + 1
			continue
		}
		for {
			j := len(s)
			if k := strings.IndexAny(s[i:], notInAName); k >= 0 {
				j = i + k
			}
			if j < len(s) && s[j] == '\\' {
				if j+1 < len(s) && (s[j+1] == 'n' || s[j+1] == 't') && pathFileRe.MatchString(strings.TrimRight(s[i:j], " ")) {
					i = j
					break
				}
				i = j + 1
				continue
			}
			i = j
			break
		}
		spans = append(spans, [2]int{begin, i})
		pos = i
		if pos <= begin {
			pos = begin + 1
		}
	}
	return spans
}

func isASCIIAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// outsidePaths is s with change applied to every stretch that is not path-looking text.
func outsidePaths(s string, change func(string) string) string {
	var b strings.Builder
	last := 0
	for _, span := range pathSpans(s) {
		b.WriteString(change(s[last:span[0]]))
		b.WriteString(s[span[0]:span[1]])
		last = span[1]
	}
	b.WriteString(change(s[last:]))
	return b.String()
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

// unstartedScreenNudge is what to tell the operator instead of taking a DONE
// for work in a window when no command has run at all, or "" to take it.
// Found by the GUI benchmark, on a hosted brain: asked to rename a sheet in
// the spreadsheet on the screen, the operator's first and only reply was
// "DONE: Renamed the sheet from Sheet1 to Budget 2026 and saved the file.
// Verified in Calc that the tab now reads Budget 2026" - three seconds, not
// one command, the sheet untouched. It was its answer to the same request an
// hour before, reworded, so earlyDoneNudge (which looks for the same words)
// let it through, and nothing checks a task that never touched the screen.
// Work in a window cannot be done, or known to be done, without looking: a
// request the triage sends here as screen work needs at least one command.
//
// The same holds for work on a file the request names. Found live
// (2026-10-12): the fourth "summarize sales_<month>.csv into
// summary_<month>.xlsx" of a conversation was answered in three seconds with
// "summary_feb.xlsx is complete ... I verified the workbook by reading it
// back", figures and all - no command run, no file written, the totals
// invented. The words were new, the file was named without a path (so it was
// no "document" of the request), and a task that wrote no file has nothing a
// final check could read back.
func unstartedScreenNudge(st *operatorState) string {
	if st.CommandsRun > 0 {
		return ""
	}
	if !atomizer.WantsScreen(st.Request) {
		if !namesFileWork(st.Request) {
			return ""
		}
		return "Not taken: nothing has been run for this request, so no file has been read, written or changed " +
			"for it. Whatever you remember from earlier in this conversation, the files are as they are NOW, and " +
			"an answer about a file comes only from a command that read or wrote it. Do what is asked, with " +
			"commands:\n" + st.Request
	}
	return "Not taken: nothing has been run for this request, so nothing on this screen has been looked at or " +
		"changed. Whatever you remember from earlier in this conversation, the screen and the files are as they " +
		"are NOW. Look first, then do what is asked:\n" + st.Request + "\n\nReply now with exactly:\nCOMMAND:\ngui observe"
}

var (
	// fileNameRe is a file named in a request, with or without a path.
	fileNameRe = regexp.MustCompile(`(?i)(?:^|[\s"'(=])[\w~./\\-]*[\w-]\.(?:csv|tsv|xlsx|xlsm|xls|ods|docx|doc|odt|pdf|pptx|txt|md|json|xml|ya?ml|html?|eml|log|ini|conf|py|sh|sql|png|jpe?g|zip)\b`)
	// fileWorkRe is something to be done with one, as against asked about one.
	fileWorkRe = regexp.MustCompile(`(?i)\b(creat\w*|writ\w*|sav\w*|summari[sz]\w*|updat\w*|convert\w*|combin\w*|read\w*|extract\w*|total\w*|merg\w*|append\w*|generat\w*|export\w*|fill\w*|compar\w*|into|renam\w*|delet\w*|remov\w*|mov\w*|cop(?:y|ies|ied)|edit\w*|chang\w*|add\w*|build\w*|mak\w*|count\w*|sort\w*|filter\w*|clean\w*|pars\w*|load\w*|import\w*)\b`)
)

// namesFileWork reports whether a request names a file and says to do
// something with it: work that cannot be done, or known to be done, without a
// command. "what is a .csv file?" names no file; "is notes.txt a good name?"
// asks for nothing to be done to it.
func namesFileWork(request string) bool {
	own, _, _ := strings.Cut(request, attachedFilesMark)
	return fileNameRe.MatchString(own) && fileWorkRe.MatchString(own)
}

// unstartedScreenDoubt is what a person is asked when the operator says done
// again and still has run nothing.
func unstartedScreenDoubt(claim string) string {
	return fmt.Sprintf("I cannot confirm this is done: I reported %q, but I ran no command for this request, so "+
		"nothing on the screen or in any file was looked at or changed. Reply \"done\" if it really is done "+
		"already, or tell me to do it.", claim)
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
// maxSlipNudges is how many commands holding a stray writing system are sent back before the ordinary checks decide.
const maxSlipNudges = 3

// scripts are the writing systems a slip is told apart by (Latin, digits and punctuation are never a slip).
var scripts = []*unicode.RangeTable{unicode.Thai, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul,
	unicode.Cyrillic, unicode.Arabic, unicode.Hebrew, unicode.Devanagari, unicode.Bengali, unicode.Tamil,
	unicode.Telugu, unicode.Greek, unicode.Georgian, unicode.Armenian, unicode.Ethiopic, unicode.Khmer, unicode.Lao,
	unicode.Myanmar, unicode.Sinhala, unicode.Gujarati, unicode.Gurmukhi, unicode.Kannada, unicode.Malayalam}

// madeBehindRe is a command that writes such a file without the app: office's own create/update, a script that
// saves a workbook, deck, document or picture, a headless conversion, ImageMagick.
var madeBehindRe = regexp.MustCompile(`(?is)\boffice\s+(?:create|update)\b|\.save\(|\bsoffice\b[^\n]*--convert-to|\b(?:convert|mogrify|magick)\s`)

// strayScript is what to tell the operator instead of running a command whose typed text uses a writing system
// that nothing in the task - the request, what commands have shown - uses, or "" when there is none.
func strayScript(st *operatorState, command string) string {
	values := typedValues(command)
	if len(values) == 0 {
		return ""
	}
	known := st.Request + "\n" + st.Seen
	for _, v := range values {
		for _, table := range scripts {
			if containsScript(v, table) && !containsScript(known, table) {
				return fmt.Sprintf("Not run: %q is written in a script that nothing in this task uses - it is not part "+
					"of the work, and was a slip. Send the command again with the value as the request or the "+
					"document gives it.", v)
			}
		}
	}
	return ""
}

func containsScript(s string, table *unicode.RangeTable) bool {
	for _, r := range s {
		if unicode.Is(table, r) {
			return true
		}
	}
	return false
}

// pictureExtRe is a document that is a picture: what it says is seen, never parsed.
var pictureExtRe = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|bmp|tiff?|webp|heic)$`)

// unseenPictures is the request's pictures that no `office read` has shown the model. Found live (2026-10-14): a
// phone photo of a clear invoice was registered "illegible" by a worker that only ever opened it with a Python
// script - which reads pixels, not words - and never looked at it.
func unseenPictures(st *operatorState) []string {
	var out []string
	for _, doc := range requestDocuments(st.Request) {
		if !pictureExtRe.MatchString(doc) {
			continue
		}
		base := docName(doc)
		seen := false
		for _, c := range st.Commands {
			if strings.Contains(c, "office read") && (strings.Contains(c, doc) || strings.Contains(c, base)) {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, doc)
		}
	}
	return out
}

func unseenPicturesNudge(unseen []string) string {
	return fmt.Sprintf("Not taken yet: you were given %s and have not looked at it - a script that opens a picture's "+
		"pixels cannot tell what it says. Look at it with office read %s (it is shown to you), then finish with what "+
		"it says.", strings.Join(unseen, ", "), unseen[0])
}

// docName is a document's file name: what follows its last / or \.
func docName(doc string) string { return doc[strings.LastIndexAny(doc, `/\`)+1:] }

func readBefore(commands []string, doc string) bool {
	base := docName(doc)
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
// separators - written one way (6,000.00, Rs. 6000 and 6000 are all "6000"), or false. Letters before the digits are
// taken only when they are a currency's code or abbreviation: "INV 5000", "PO 12" and "Bill 456" are identifiers,
// never the numbers 5000, 12 and 456.
func canonicalNumber(v string) (string, bool) {
	v = withoutCurrencyMark(strings.TrimSpace(v))
	if !numberRe.MatchString(v) || numberRe.FindString(v) != v {
		return "", false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(f, 'f', -1, 64), true
}

// currencyWords are the currency codes and abbreviations written before an amount (ISO 4217 codes and their common
// short forms), lowercased and without dots.
var currencyWords = map[string]bool{
	"rs": true, "inr": true, "re": true, "usd": true, "eur": true, "gbp": true, "jpy": true, "cny": true,
	"rmb": true, "aud": true, "cad": true, "chf": true, "sgd": true, "aed": true, "sar": true, "qar": true, "kwd": true,
	"hkd": true, "nzd": true, "zar": true, "sek": true, "nok": true, "dkk": true, "pln": true, "czk": true, "huf": true,
	"rub": true, "brl": true, "mxn": true, "krw": true, "try": true, "pkr": true, "lkr": true, "npr": true, "bdt": true,
	"idr": true, "rp": true, "myr": true, "rm": true, "thb": true, "php": true, "vnd": true, "egp": true, "ngn": true,
	"kes": true, "fr": true, "sfr": true,
}

// dollarPrefixes name a dollar only written against its sign: US$, A$, C$, S$, NZ$, HK$.
var dollarPrefixes = map[string]bool{"us": true, "a": true, "c": true, "s": true, "nz": true, "hk": true}

// withoutCurrencyMark is v with any currency mark before its digits taken off - symbols (₹ $ € £ ¥) and the words in
// currencyWords ("Rs.", "INR", "US$") - or v unchanged from the first letters that are no currency's.
func withoutCurrencyMark(v string) string {
	for v != "" {
		r, size := utf8.DecodeRuneInString(v)
		switch {
		case unicode.IsDigit(r):
			return v
		case unicode.IsSymbol(r) || unicode.IsSpace(r) || r == '.':
			v = v[size:]
		case unicode.IsLetter(r):
			end := strings.IndexFunc(v, func(r rune) bool { return !unicode.IsLetter(r) })
			if end < 0 {
				return v
			}
			word := strings.ToLower(v[:end])
			if !currencyWords[word] && !(dollarPrefixes[word] && strings.HasPrefix(v[end:], "$")) {
				return v
			}
			v = v[end:]
		default:
			return v
		}
	}
	return v
}

// numbersIn is every number written in text, each in canonicalNumber's form. A bill writes a rate as "Rs. 6,000.00"
// and the form it goes into takes 6000: found live, the plain text match refused the chair's rate on an application bill and
// the operator stopped to ask whether to type the bill's own figure.
// A figure is a whole one: the digits after a figure's own point or comma ("1.2.3" -> the "3") are part of it, not a
// figure of their own. (Digits after letters are kept: a cell's row in "B2" is how a sheet shows the column index a
// lookup formula types.)
func numbersIn(text string) map[string]bool {
	out := map[string]bool{}
	for _, loc := range numberRe.FindAllStringIndex(text, -1) {
		if !wholeFigureAt(text, loc[0]) {
			continue
		}
		if n, ok := canonicalNumber(text[loc[0]:loc[1]]); ok {
			out[n] = true
		}
	}
	return out
}

// wholeFigureAt reports whether the digits at text[start:] begin a figure of their own (numbersIn).
func wholeFigureAt(text string, start int) bool {
	if start == 0 {
		return true
	}
	prev, size := utf8.DecodeLastRuneInString(text[:start])
	if prev == '.' || prev == ',' {
		before, _ := utf8.DecodeLastRuneInString(text[:start-size])
		return !unicode.IsDigit(before)
	}
	return true
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

// datesIn is every date written in text. A bill writes "04-Oct-2026" and the web app's date field takes 04-10-2026:
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

// boundsAMonth says whether v is the first or last day of a month some date the document writes falls in: the edges
// of the period the work is about (a report's from and to), worked out, not invented. Found live (2026-10-25): asked to
// check a month's entries, the worker could not type 1-11-2026 and 30-11-2026 into a day book's filter and asked the
// person for "the exact dates".
func (s *dateSet) boundsAMonth(v dateValue) bool {
	// Each reading of v: 0 unambiguous, 1 day first, 2 month first. An ambiguous v is set only against the document's
	// dates read the same way round, so a swapped day and month never traces.
	type reading struct{ y, m, d, way int }
	var readings []reading
	for _, day := range v.days {
		var r reading
		if _, err := fmt.Sscanf(day, "%d-%d-%d", &r.y, &r.m, &r.d); err == nil {
			readings = append(readings, r)
		}
	}
	if v.order != "" {
		var a, b, y int
		if _, err := fmt.Sscanf(v.order, "%d/%d/%d", &a, &b, &y); err == nil {
			readings = append(readings, reading{y, b, a, 1}, reading{y, a, b, 2})
		}
	}
	monthWritten := func(y, m, way int) bool {
		prefix := fmt.Sprintf("%04d-%02d-", y, m)
		for seen := range s.days {
			if strings.HasPrefix(seen, prefix) {
				return true
			}
		}
		for o := range s.orders {
			var a, b, oy int
			if _, err := fmt.Sscanf(o, "%d/%d/%d", &a, &b, &oy); err != nil || oy != y {
				continue
			}
			if (way != 2 && b == m) || (way != 1 && a == m) {
				return true
			}
		}
		return false
	}
	for _, r := range readings {
		last := time.Date(r.y, time.Month(r.m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
		if (r.d == 1 || r.d == last) && monthWritten(r.y, r.m, r.way) {
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
			// Inside double quotes a backslash escapes only $ ` " \ and a newline, as in the shell; before anything
			// else it is itself. Found live (2026-10-15): "C:\Users\Administrator\Documents" lost its backslashes
			// here, and the folder path the request gave was refused as a value nobody had given.
			if quote == '"' && !strings.ContainsRune("$`\"\\\n", r) {
				cur.WriteRune('\\')
			}
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

// lookupRe is a command that runs the web lookup tool (agent/lookup).
var lookupRe = regexp.MustCompile(`(?m)(^|[;&|(]\s*)lookup\s`)

// isLookup reports whether a command looks something up on the web.
func isLookup(command string) bool { return lookupRe.MatchString(command) }

// lookedUp reports whether this task has looked anything up on the web.
func lookedUp(st *operatorState) bool {
	for _, c := range st.Commands {
		if isLookup(c) {
			return true
		}
	}
	return false
}

// researchRe is a request whose answer is meant to come from the web: what it finds there may be typed.
var researchRe = regexp.MustCompile(`(?i)\b(look(?:ing)? up|search(?:ing)? (?:for|the web|online)|research|find out|google|on the (?:web|internet)|online)\b`)

// howToQuestionRe is a question about how an app works - something a page of its documentation can answer - not
// about what the person wants.
var howToQuestionRe = regexp.MustCompile(`(?i)\b(how (?:do|can|should|to) (?:i|we|you)?\s*\w+|where (?:is|are|do|can) .{0,40}\b(?:menu|button|option|setting|field|screen|tab|dialog)|which (?:menu|button|option|key|screen|tab|shortcut)|(?:can't|cannot|could not|couldn't|unable to|don't know how to) (?:find|locate|see|tell)\b.{0,40}\b(?:menu|button|option|setting|field|screen|tab|dialog|way)|not sure how|(?:clear|dismiss|close|get past|fill(?: in)?|complete|answer|handle) (?:that|this|the) .{0,40}\b(?:popup|pop-up|dialog|prompt|sub-?screen|screen|window|panel|form)\b)`)

// lookupNudge answers a question about how an app works asked before anything was looked up.
const lookupNudge = "Not asked yet: that is a question about how the app works, and its documentation can answer it. " +
	"Look it up first - lookup \"APP how to WHAT\" (it reads the web without touching the screen) - then carry on. " +
	"Ask the person only what the web cannot know: their choices, their data, a password."
