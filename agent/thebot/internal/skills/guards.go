// guards.go: the operator loop's mechanical guards on what a command costs the transcript and whether it may run -
// a step that writes something (it resets the cycle guard, cycle.go), the stub for an output byte-identical to one the
// transcript still shows, the screenshot not sent again while the screen is unchanged, the command cut off before its
// end, and the 40/60 cut of a long output with a note when a JSON listing says it has more. Ported in our own style
// from hermes-agent's agent/tool_guardrails.py (identical-result stub), tools/computer_use/tool.py
// (_screenshot_dedup_check), agent/turn_tool_validation.py (truncated arguments), tools/tool_output_truncate.py
// (head/tail split) and agent/tool_dispatch_helpers.py (elision notice). Every decision here is a fact of bytes or of
// shell syntax, never of the words in an output.
package skills

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"thebot/internal/llm"
)

// guardState is what these guards keep across a goal's turns (embedded in operatorState, so a checkpoint keeps it).
type guardState struct {
	// SeenOutputs is, by command (spacing aside), the last output of it the transcript shows whole (identicalOutputStub).
	SeenOutputs map[string]seenOutput `json:",omitempty"`
	// ShotSum is the hash of the last screenshot attached, ShotWindow the window then active, ShotSentStep its step,
	// ShotMsg its message and ShotStreak how many identical screenshots since were not sent (attachScreenshot).
	ShotSum      string `json:",omitempty"`
	ShotWindow   string `json:",omitempty"`
	ShotSentStep int    `json:",omitempty"`
	ShotMsg      int    `json:",omitempty"`
	ShotStreak   int    `json:",omitempty"`
	// CutCommandNudges counts the commands in a row sent back as cut off (incompleteCommand).
	CutCommandNudges int `json:",omitempty"`
}

// seenOutput is one command's output as shown whole: its step, its hash and size, and the message showing it with the
// length and hash of that message's start (the formatted result), so a later change to the message is noticed.
type seenOutput struct {
	Step   int
	Sum    string
	Bytes  int
	Msg    int
	Len    int
	MsgSum string
}

const (
	// minStubBytes is the shortest output worth a stub (hermes-agent's IDENTICAL_RESULT_STUB_MIN_CHARS).
	minStubBytes = 512
	// maxSeenOutputs bounds SeenOutputs.
	maxSeenOutputs = 200
	// maxShotStreak is how many identical screenshots in a row are not sent; the next one is sent anyway.
	maxShotStreak = 2
	// maxCutCommandNudges is how many cut-off commands in a row are sent back; the next one runs, and the shell says
	// what it makes of it (a false reading of the syntax must never stall the goal).
	maxCutCommandNudges = 2
	// headShare is the part of a cut output kept from its start; the rest of the room goes to its end, where the
	// newest lines and a command's last word (an error, a pipe's failure note) are.
	headShare = 0.4
)

// sumOf is the hex SHA-256 of b.
func sumOf(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// canonicalCommand is a command with its spacing made single, the key the guards compare commands by.
func canonicalCommand(command string) string { return strings.Join(strings.Fields(command), " ") }

// ---- the step that writes something ----

// entersDataRe is a gui command that enters or moves data, or runs a taught task that does; officeWriteRe an office
// command that writes a file.
var (
	entersDataRe  = regexp.MustCompile(`^\s*gui\s+(type|select|upload|drag|do|replay)\b`)
	officeWriteRe = regexp.MustCompile(`^\s*office\s+(update|create)\b`)
)

// changesState reports whether a command writes something, by its structure: a gui verb that enters or moves data
// (type, select, upload, drag) or runs a taught task (do, replay), office update or create, or a redirection or tee
// into a file (not /dev/null, not a descriptor). A round of such commands can print the same each time and still move
// the work on (a row entered per round), so it is never taken for a cycle. Moving about - click, key, open, scroll,
// observe - is not: a round of those that prints exactly what it printed the round before has changed nothing.
func changesState(command string) bool {
	for _, seg := range shellSegments(command) {
		if entersDataRe.MatchString(seg.Text) || officeWriteRe.MatchString(seg.Text) {
			return true
		}
		for _, m := range writeTargetRe.FindAllStringSubmatch(seg.Text, -1) {
			if f := m[1] + m[2]; f != "" && f != "/dev/null" && !strings.HasPrefix(f, "&") {
				return true
			}
		}
	}
	return false
}

// ---- the identical-output stub ----

// identicalOutputStub is what stands for a command's output when the same command printed exactly this before and the
// transcript still shows that output whole: "" otherwise, and for a failure, a timeout or an output under
// minStubBytes. The command still ran; only the newest message is shorter, so nothing already sent changes.
func (st *operatorState) identicalOutputStub(command string, res shellResult) string {
	if res.ExitCode != 0 || res.TimedOut || len(res.Output) < minStubBytes {
		return ""
	}
	seen, ok := st.SeenOutputs[canonicalCommand(command)]
	if !ok || seen.Sum != sumOf([]byte(res.Output)) || !st.stillShown(seen) {
		return ""
	}
	if listsScreen(command) {
		return fmt.Sprintf("The screen listing is unchanged since step %d (%d bytes, byte for byte the same); its refs still hold.",
			seen.Step, seen.Bytes)
	}
	return fmt.Sprintf("Output identical to step %d's output (%d bytes); unchanged.", seen.Step, seen.Bytes)
}

// stillShown reports whether the message that showed an output whole still starts as it did.
func (st *operatorState) stillShown(seen seenOutput) bool {
	if seen.Msg < 0 || seen.Msg >= len(st.Messages) {
		return false
	}
	content := st.Messages[seen.Msg].Content
	return len(content) >= seen.Len && sumOf([]byte(content[:seen.Len])) == seen.MsgSum
}

// noteOutputShown keeps, after the newest message (st.Messages' last) was appended for command, what a later identical
// output may point to: the output it shows whole (head is the formatted result the message starts with). A stubbed
// screen listing instead marks the listing it points to as the newest screen again, so neither the local digest nor a
// hosted maintenance pass shortens the listing whose refs are still the screen's.
func (st *operatorState) noteOutputShown(command string, res shellResult, head string, stubbed bool) {
	key := canonicalCommand(command)
	if stubbed {
		if seen, ok := st.SeenOutputs[key]; ok && listsScreen(command) {
			for i, idx := range st.ScreenMsgs {
				if idx == seen.Msg {
					st.ScreenMsgs = append(append(st.ScreenMsgs[:i:i], st.ScreenMsgs[i+1:]...), idx)
					break
				}
			}
		}
		return
	}
	if res.ExitCode != 0 || res.TimedOut || len(res.Output) < minStubBytes || len(st.Messages) == 0 {
		delete(st.SeenOutputs, key)
		return
	}
	msg := len(st.Messages) - 1
	content := st.Messages[msg].Content
	if !strings.HasPrefix(content, head) {
		return
	}
	if st.SeenOutputs == nil {
		st.SeenOutputs = map[string]seenOutput{}
	}
	if _, ok := st.SeenOutputs[key]; !ok && len(st.SeenOutputs) >= maxSeenOutputs {
		oldest, at := "", 0
		for k, s := range st.SeenOutputs {
			if oldest == "" || s.Step < at {
				oldest, at = k, s.Step
			}
		}
		delete(st.SeenOutputs, oldest)
	}
	st.SeenOutputs[key] = seenOutput{Step: st.Step, Sum: sumOf([]byte(res.Output)), Bytes: len(res.Output), Msg: msg,
		Len: len(head), MsgSum: sumOf([]byte(head))}
}

// ---- the screenshot not sent again ----

// screenUnchangedNote is what a command's result says in place of a screenshot byte-identical to the last one sent.
// (It never ends with screenshotNote: that suffix marks a message that carries the picture.)
const screenUnchangedNote = "\n\n(Screen unchanged since step %d: this command left it byte for byte as that step's " +
	"screenshot shows, so the picture is not sent again.)"

// attachScreenshot puts the screenshot a command left on its result msg (appended next, at len(st.Messages)) - or,
// when it is byte-identical to the last one sent, for the same window, and that one is still in the transcript, the
// note that the screen is unchanged since its step, at most maxShotStreak times in a row (the next is sent anyway, as
// in hermes-agent's _screenshot_dedup_check). An unchanged screen takes no earlier picture off the transcript either.
func (st *operatorState) attachScreenshot(msg *llm.Message, shot []byte) {
	sum := sumOf(shot)
	if sum == st.ShotSum && st.ActiveWindow == st.ShotWindow && st.ShotStreak < maxShotStreak && st.shotStillShown() {
		st.ShotStreak++
		msg.Content += fmt.Sprintf(screenUnchangedNote, st.ShotSentStep)
		st.ShotStep = st.Step // the picture in the transcript is the screen as this command left it
		return
	}
	if !st.Hosted {
		st.forgetOldPictures(screenshotNote, 0)
	}
	msg.Images, msg.ImageLabels = [][]byte{shot}, []string{"the screen as this command left it"}
	msg.Content += screenshotNote
	st.ShotStep = st.Step
	st.ShotSum, st.ShotWindow, st.ShotSentStep, st.ShotMsg, st.ShotStreak = sum, st.ActiveWindow, st.Step, len(st.Messages), 0
}

// shotStillShown reports whether the last screenshot sent is still on its message.
func (st *operatorState) shotStillShown() bool {
	if st.ShotMsg < 0 || st.ShotMsg >= len(st.Messages) {
		return false
	}
	m := st.Messages[st.ShotMsg]
	return len(m.Images) > 0 && strings.HasSuffix(m.Content, screenshotNote) && sumOf(m.Images[len(m.Images)-1]) == st.ShotSum
}

// ---- the command cut off before its end ----

// cutCommandNudge answers a command that is not whole.
const cutCommandNudge = "Your command was cut off before its end (%s), so it was not run. Send the whole command again " +
	"with COMMAND. If it is long, write it in parts: a script file first (a here-document closed by its word), then run it."

// shell lexer frames: the command line itself, $( ), a bare ( ), $(( )), ` `, and " ".
const (
	frameTop = iota
	frameSubst
	frameParen
	frameArith
	frameTick
	frameQuote
)

// lexFrame is one open construct; cases counts the case statements open in it (their patterns' ")" close nothing),
// depth the bare parentheses open inside an arithmetic expansion.
type lexFrame struct {
	kind  int
	cases int
	depth int
}

// pendingHeredoc is a here-document opened on the current line: its closing word, and whether leading tabs go (<<-).
type pendingHeredoc struct {
	word  string
	strip bool
}

// incompleteCommand is why a command line is not whole by the shell's syntax alone, or "": a quote left open, a
// here-document never closed by its word, an unclosed $( ), $(( )), ( ) or backtick. Comments, here-document bodies
// and single quotes are read as the shell reads them, so an apostrophe in those is no quote. Anything it cannot place
// for certain (a ")" with nothing open, a case pattern) is let through: the shell then says what it makes of it.
func incompleteCommand(command string) string {
	r := []rune(command)
	n := len(r)
	stack := []lexFrame{{kind: frameTop}}
	var heredocs []pendingHeredoc
	top := func() *lexFrame { return &stack[len(stack)-1] }
	wordStart := func(i int) bool {
		if i == 0 {
			return true
		}
		switch r[i-1] {
		case ' ', '\t', '\n', ';', '&', '|', '(', ')', '`':
			return true
		}
		return false
	}
	for i := 0; i < n; i++ {
		c := r[i]
		f := top()
		if f.kind == frameQuote {
			switch {
			case c == '\\':
				i++
			case c == '"':
				stack = stack[:len(stack)-1]
			case c == '`':
				stack = append(stack, lexFrame{kind: frameTick})
			case c == '$' && i+2 < n && r[i+1] == '(' && r[i+2] == '(':
				stack = append(stack, lexFrame{kind: frameArith})
				i += 2
			case c == '$' && i+1 < n && r[i+1] == '(':
				stack = append(stack, lexFrame{kind: frameSubst})
				i++
			}
			continue
		}
		if f.kind == frameArith {
			switch {
			case c == '(':
				f.depth++
			case c == ')' && f.depth > 0:
				f.depth--
			case c == ')' && i+1 < n && r[i+1] == ')':
				stack = stack[:len(stack)-1]
				i++
			case c == ')':
				// "$((cmd) ...)" was a command substitution holding a subshell: this ")" closed the subshell.
				f.kind = frameSubst
			case c == '$' && i+1 < n && r[i+1] == '(':
				stack = append(stack, lexFrame{kind: frameSubst})
				i++
			case c == '`':
				stack = append(stack, lexFrame{kind: frameTick})
			case c == '"':
				stack = append(stack, lexFrame{kind: frameQuote})
			case c == '\'':
				j := closingSingle(r, i+1, false)
				if j < 0 {
					return "a ' quote is not closed"
				}
				i = j
			}
			continue
		}
		// A code frame: the command line, $( ), ( ) or ` `.
		switch {
		case c == '\\':
			i++
		case c == '#' && wordStart(i):
			for i+1 < n && r[i+1] != '\n' {
				i++
			}
		case c == '\'':
			ansi := i > 0 && r[i-1] == '$'
			j := closingSingle(r, i+1, ansi)
			if j < 0 {
				return "a ' quote is not closed"
			}
			i = j
		case c == '"':
			stack = append(stack, lexFrame{kind: frameQuote})
		case c == '`':
			if f.kind == frameTick {
				stack = stack[:len(stack)-1]
			} else {
				stack = append(stack, lexFrame{kind: frameTick})
			}
		case c == '$' && i+2 < n && r[i+1] == '(' && r[i+2] == '(':
			stack = append(stack, lexFrame{kind: frameArith})
			i += 2
		case c == '$' && i+1 < n && r[i+1] == '(':
			stack = append(stack, lexFrame{kind: frameSubst})
			i++
		case c == '(' && i+1 < n && r[i+1] == '(' && wordStart(i):
			stack = append(stack, lexFrame{kind: frameArith})
			i++
		case c == '(':
			stack = append(stack, lexFrame{kind: frameParen})
		case c == ')':
			if f.cases > 0 {
				continue // a case pattern's end
			}
			if f.kind == frameSubst || f.kind == frameParen {
				stack = stack[:len(stack)-1]
			}
		case c == '<' && i+1 < n && r[i+1] == '<':
			if i+2 < n && r[i+2] == '<' {
				i += 2 // a here-string
				continue
			}
			h, end := heredocWord(r, i+2)
			if h.word != "" {
				heredocs = append(heredocs, h)
			}
			i = end - 1
		case c == '\n' && len(heredocs) > 0:
			for _, h := range heredocs {
				j, ok := heredocEnd(r, i+1, h)
				if !ok {
					return "the here-document opened with <<" + h.word + " is never closed by a line " + h.word
				}
				i = j
			}
			heredocs = nil
		case isLetter(c) && commandPosition(r, i):
			j := i
			for j < n && (isLetter(r[j]) || r[j] == '_' || (r[j] >= '0' && r[j] <= '9')) {
				j++
			}
			if j == n || strings.ContainsRune(" \t\n;&|()", r[j]) {
				switch string(r[i:j]) {
				case "case":
					f.cases++
				case "esac":
					if f.cases > 0 {
						f.cases--
					}
				}
			}
			i = j - 1
		}
	}
	if len(heredocs) > 0 {
		return "the here-document opened with <<" + heredocs[0].word + " is never closed by a line " + heredocs[0].word
	}
	switch top().kind {
	case frameQuote:
		return `a " quote is not closed`
	case frameTick:
		return "a ` is not closed"
	case frameSubst:
		return "a $( is not closed by its )"
	case frameArith:
		return "a (( is not closed by its ))"
	case frameParen:
		return "a ( is not closed by its )"
	}
	return ""
}

// commandPosition reports whether the word at i stands where a command begins: at the start, or after blanks that
// follow a separator (; & | ( a newline or a backtick). Only there are case and esac read as keywords: a "case" taken
// for one by mistake would leave a ")" unread and the line taken for cut off; one missed only lets a line through.
func commandPosition(r []rune, i int) bool {
	if i > 0 && (isLetter(r[i-1]) || r[i-1] == '_' || (r[i-1] >= '0' && r[i-1] <= '9')) {
		return false
	}
	j := i - 1
	for j >= 0 && (r[j] == ' ' || r[j] == '\t') {
		j--
	}
	return j < 0 || strings.ContainsRune(";&|(\n`", r[j])
}

// isLetter reports whether c is an ASCII letter (a shell keyword's).
func isLetter(c rune) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// closingSingle is the index of the ' that closes a single-quoted string starting at from, or -1; in $'...' a
// backslash escapes the next character.
func closingSingle(r []rune, from int, ansi bool) int {
	for j := from; j < len(r); j++ {
		if ansi && r[j] == '\\' {
			j++
			continue
		}
		if r[j] == '\'' {
			return j
		}
	}
	return -1
}

// heredocWord reads the here-document word after "<<" at from (an optional "-", blanks, then a word, any part of it
// quoted or escaped): the word without its quotes, and the index after it. An empty word is no here-document.
func heredocWord(r []rune, from int) (pendingHeredoc, int) {
	i := from
	h := pendingHeredoc{}
	if i < len(r) && r[i] == '-' {
		h.strip = true
		i++
	}
	for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
		i++
	}
	var w strings.Builder
	for i < len(r) && !strings.ContainsRune(" \t\n;&|()<>", r[i]) {
		switch r[i] {
		case '\'', '"':
			q := r[i]
			j := i + 1
			for j < len(r) && r[j] != q {
				j++
			}
			if j >= len(r) {
				return pendingHeredoc{}, i // an unclosed quote: the caller's scan reports it
			}
			w.WriteString(string(r[i+1 : j]))
			i = j + 1
		case '\\':
			if i+1 < len(r) {
				w.WriteRune(r[i+1])
			}
			i += 2
		default:
			w.WriteRune(r[i])
			i++
		}
	}
	h.word = w.String()
	return h, i
}

// heredocEnd finds the line closing here-document h in r from the start of a line at from: the index of the newline
// ending that line (or len(r)-1 when it is the last), and whether one was found. Trailing blanks and a carriage
// return on the closing line are let pass.
func heredocEnd(r []rune, from int, h pendingHeredoc) (int, bool) {
	for start := from; start <= len(r); {
		end := start
		for end < len(r) && r[end] != '\n' {
			end++
		}
		line := string(r[start:end])
		if h.strip {
			line = strings.TrimLeft(line, "\t")
		}
		if strings.TrimRight(line, " \t\r") == h.word {
			if end >= len(r) {
				return len(r) - 1, true
			}
			return end, true
		}
		if end >= len(r) {
			break
		}
		start = end + 1
	}
	return 0, false
}

// ---- the cut of a long output, and the listing that says it has more ----

// cutOutput is out as the model is shown it within limit bytes: whole when it fits, else its first 40% and its last
// 60% (each at a whole character) around one marker that says how many bytes were left out of how many, and where the
// whole text is when it was kept (fullPath).
func cutOutput(out string, limit int, fullPath string) string {
	if limit <= 0 || len(out) <= limit {
		return out
	}
	head := int(float64(limit) * headShare)
	for head > 0 && !utf8.RuneStart(out[head]) {
		head--
	}
	tail := len(out) - (limit - head)
	for tail < len(out) && !utf8.RuneStart(out[tail]) {
		tail++
	}
	where := ""
	if fullPath != "" {
		where = "; the whole output is in " + fullPath + " - grep or sed -n it for the part you need"
	}
	return out[:head] + fmt.Sprintf("\n\n... [%d bytes left out of %d%s] ...\n\n", tail-head, len(out), where) + out[tail:]
}

// keptOutputPath is where the whole of a command's output is on disk when the model is shown it cut (longer than
// limit): the file the ledger just kept it in (noteLedger, the newest line), else one kept now; "" when it fits or
// could not be kept.
func (st *operatorState) keptOutputPath(res shellResult, limit int) string {
	if len(res.Output) <= limit {
		return ""
	}
	const mark = " (full output: "
	if n := len(st.Ledger); n > 0 && strings.HasSuffix(st.Ledger[n-1], ")") {
		line := st.Ledger[n-1]
		if i := strings.LastIndex(line, mark); i >= 0 {
			if p := strings.TrimSuffix(line[i+len(mark):], ")"); st.OutputsDir != "" && strings.HasPrefix(p, st.OutputsDir) {
				return p // (only a file of this goal's own: never text the command printed that looks like one)
			}
		}
	}
	return st.keepOutput(res.Output)
}

// incompleteListingNote is said beside an output that is JSON whose own keys say it shows only part of a listing.
const incompleteListingNote = "\n\n(INCOMPLETE: this listing says more items exist than it shows (%s). Fetch the rest " +
	"before treating it as the whole list.)"

// listingIncompleteNote is incompleteListingNote when out, as a whole, is a JSON value carrying "has_more": true or
// "truncated": true at any depth (a key of the structure, never a phrase of the text); "" otherwise.
func listingIncompleteNote(out string) string {
	t := strings.TrimSpace(out)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return ""
	}
	var v any
	if json.Unmarshal([]byte(t), &v) != nil {
		return ""
	}
	if key := moreKey(v, 0); key != "" {
		return fmt.Sprintf(incompleteListingNote, `"`+key+`": true`)
	}
	return ""
}

// moreKey is the first of "has_more" or "truncated" set to true in v, looked for down to a depth of 6.
func moreKey(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range []string{"has_more", "truncated"} {
			if b, ok := x[k].(bool); ok && b {
				return k
			}
		}
		for _, e := range x {
			if k := moreKey(e, depth+1); k != "" {
				return k
			}
		}
	case []any:
		for _, e := range x {
			if k := moreKey(e, depth+1); k != "" {
				return k
			}
		}
	}
	return ""
}
