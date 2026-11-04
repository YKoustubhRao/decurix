package skills

import (
	"regexp"
	"sort"
	"strings"
)

// TypedTextMark is what the text a gui type command types is shown as, wherever soul shows the command.
const TypedTextMark = "[text]"

// guiTypeRe finds a gui type command: "gui" then "type" as words, at a command's start or after a shell separator.
var guiTypeRe = regexp.MustCompile("(^|[\\s;&|()$`'\"])gui[ \\t]+type([ \\t]|$)")

// typeRefRe is a target gui type takes by itself: an element ref (e12) or a spreadsheet cell or range (D1, F3:F13).
var typeRefRe = regexp.MustCompile(`^(e\d+|[A-Z]{1,3}\d+(:[A-Z]{1,3}\d+)?)$`)

// typeValueFlags are gui type's flags that take a value: a way of naming the target, never the text.
var typeValueFlags = map[string]bool{"--role": true, "--name": true, "--see": true}

// MaskTypedText is s with the text every gui type command in it types shown as TypedTextMark (F-S3, defence in
// depth): the step line, the job log, the stream and the snapshot never quote what is typed, so a secret nobody named
// as one does not leave soul in them. It fails closed:
//   - the target stays (an element ref, a cell, a role and name, or --role/--name/--see), and so do the flags before
//     it; everything after them to the command's end is the text, one mark;
//   - a here-document's body or a here-string gui type reads is masked;
//   - when gui type reads its text from stdin ("-", or no text) and a pipeline feeds it, the arguments of every
//     command in that pipeline are masked too (printf [text] | gui type e3 -): their program names stay.
//
// The command that runs is never this text: only what soul shows of it.
func MaskTypedText(s string) string {
	if !strings.Contains(s, "gui") {
		return s
	}
	var spans []maskSpan
	for _, g := range findGuiTypes(s) {
		spans = append(spans, g.feeders...)
		spans = append(spans, maskSpan{g.argsAt, g.argsAt + g.consumed, g.masked})
	}
	sort.SliceStable(spans, func(a, b int) bool { return spans[a].from < spans[b].from })
	var out strings.Builder
	last := 0
	for _, sp := range spans {
		if sp.from < last {
			continue // inside a span already masked
		}
		out.WriteString(s[last:sp.from])
		out.WriteString(sp.with)
		last = sp.to
	}
	out.WriteString(s[last:])
	return out.String()
}

// maskSpan is a part of a text to show as with.
type maskSpan struct {
	from, to int
	with     string
}

// guiType is one gui type command found in a text: where its arguments start, what they are shown as and cover, the
// texts it types (quotes undone), and the spans of the commands feeding its stdin.
type guiType struct {
	argsAt, consumed int
	masked           string
	typed            []string
	feeders          []maskSpan
	fed              []string // the feeding commands' arguments, quotes undone
}

// findGuiTypes is every gui type command in s.
func findGuiTypes(s string) []guiType {
	var out []guiType
	pos := 0
	for pos <= len(s) {
		loc := guiTypeRe.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		if s[start] != 'g' {
			start++ // the separator before "gui"
		}
		at := pos + loc[1]
		a := maskTypeArgs(s[at:])
		g := guiType{argsAt: at, consumed: a.consumed, masked: a.masked, typed: a.typed}
		if a.stdin {
			g.feeders, g.fed = feeders(s, start)
		}
		out = append(out, g)
		pos = at + a.consumed
		if a.consumed == 0 {
			pos = at + 1
		}
	}
	return out
}

// feeders are the commands of the pipeline that feeds the command starting at at (the commands before it, on its
// line, joined to it by single pipes), as spans that keep each command's program name and mask its arguments, with
// those arguments' words, quotes undone. None when the command is not fed by a pipe.
func feeders(s string, at int) ([]maskSpan, []string) {
	lineStart := strings.LastIndexByte(s[:at], '\n') + 1
	line := s[lineStart:at]
	// The commands of the line, split at separators and pipes, quotes kept whole.
	type command struct {
		from, to  int
		pipeAfter bool
	}
	var cmds []command
	from, i := 0, 0
	for i < len(line) {
		c := line[i]
		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(line) && line[j] != c {
				if c == '"' && line[j] == '\\' {
					j++
				}
				j++
			}
			i = j + 1
			continue
		case c == '|' && i+1 < len(line) && line[i+1] == '|', c == '&' && i+1 < len(line) && line[i+1] == '&':
			cmds = append(cmds, command{from, i, false})
			i += 2
			from = i
			continue
		case c == '|':
			cmds = append(cmds, command{from, i, true})
			i++
			from = i
			continue
		case c == ';', c == '(', c == '`':
			cmds = append(cmds, command{from, i, false})
			i++
			from = i
			continue
		case c == '$' && (i+1 == len(line) || line[i+1] == ' ') && (i == 0 || line[i-1] == ' '):
			// a prompt ("$ command") starts a command
			cmds = append(cmds, command{from, i, false})
			i++
			from = i
			continue
		}
		i++
	}
	if strings.TrimSpace(line[min(from, len(line)):]) != "" || len(cmds) == 0 || !cmds[len(cmds)-1].pipeAfter {
		return nil, nil // the gui type is not the end of a pipeline
	}
	var spans []maskSpan
	var fed []string
	for k := len(cmds) - 1; k >= 0 && cmds[k].pipeAfter; k-- {
		c := cmds[k]
		body := line[c.from:c.to]
		lead := len(body) - len(strings.TrimLeft(body, " \t"))
		trimmed := strings.TrimRight(body[lead:], " \t")
		w, n := readWord(trimmed)
		if n == 0 || n >= len(trimmed) {
			_ = w
			continue // a program with no arguments: nothing to mask
		}
		argsFrom := lineStart + c.from + lead + n
		argsTo := lineStart + c.from + lead + len(trimmed)
		spans = append(spans, maskSpan{argsFrom, argsTo, " " + TypedTextMark})
		rest := trimmed[n:]
		for {
			rest = strings.TrimLeft(rest, " \t")
			if rest == "" {
				break
			}
			aw, an := readWord(rest)
			if an == 0 {
				rest = rest[1:]
				continue
			}
			fed = append(fed, shellUnquote(aw.raw))
			rest = rest[an:]
		}
	}
	return spans, fed
}

// shellWord is one word of a command line as written: its raw span and whether it was quoted.
type shellWord struct {
	raw    string
	quoted bool
}

// readWord reads one shell word at the start of s (no leading blank): quotes and backslashes kept as written. It stops
// at an unquoted blank, newline or separator. n is the bytes it took; 0 at a separator or the end.
func readWord(s string) (w shellWord, n int) {
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == '<' || c == '>' || c == ')' || c == '`':
			return shellWord{raw: s[:i], quoted: w.quoted}, i
		case c == '\'':
			w.quoted = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return shellWord{raw: s, quoted: true}, len(s)
			}
			i += j + 2
		case c == '"':
			w.quoted = true
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return shellWord{raw: s, quoted: true}, len(s)
			}
			i = j + 1
		case c == '\\':
			i += 2
		default:
			i++
		}
	}
	if i > len(s) {
		i = len(s)
	}
	return shellWord{raw: s[:i], quoted: w.quoted}, i
}

// piece is one word of a command with the blank before it.
type piece struct {
	lead string
	word shellWord
	end  int // where it ends in the arguments read
}

// typeArgs is one gui type's arguments as shown: the masked text, how many bytes of the arguments it covers, the texts
// it types (quotes undone; a here-document's body and a here-string included), and whether it reads stdin.
type typeArgs struct {
	masked   string
	consumed int
	typed    []string
	stdin    bool
}

// maskTypeArgs masks the text of one gui type's arguments at the start of s: everything after the target and the
// flags before it, to the command's end (a separator or a newline), and past a here-document or here-string it reads.
func maskTypeArgs(s string) typeArgs {
	var words []piece
	i := 0
	for {
		j := i
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) {
			break
		}
		w, n := readWord(s[j:])
		if n == 0 {
			break
		}
		words = append(words, piece{lead: s[i:j], word: w, end: j + n})
		i = j + n
	}
	// Flags and the target are kept; what is left is the text.
	var kept []piece
	k := 0
	for k < len(words) && strings.HasPrefix(words[k].word.raw, "--") {
		kept = append(kept, words[k])
		if typeValueFlags[words[k].word.raw] && k+1 < len(words) {
			kept = append(kept, words[k+1])
			k++
		}
		k++
	}
	plain := words[k:]
	named := len(kept) > 0 && hasValueFlag(kept)
	switch {
	case named || len(plain) == 0:
		// --role/--name/--see name the target: every plain word is text.
	case typeRefRe.MatchString(plain[0].word.raw):
		kept, plain = append(kept, plain[0]), plain[1:]
	case len(plain) == 3 && !plain[0].word.quoted && plain[1].word.quoted:
		// gui type textbox "Site title" "value": a role and a name.
		kept, plain = append(kept, plain[0], plain[1]), plain[2:]
	case len(plain) >= 2:
		kept, plain = append(kept, plain[0]), plain[1:]
	}
	text := plain
	consumed := i
	var typed []string
	stdin := len(text) == 0 || (len(text) == 1 && text[0].word.raw == "-")
	if !stdin {
		var words []string
		for _, p := range text {
			words = append(words, shellUnquote(p.word.raw))
		}
		typed = append(typed, strings.Join(words, " "))
		if len(words) > 1 {
			typed = append(typed, words...)
		}
	}

	var b strings.Builder
	for _, p := range kept {
		b.WriteString(p.lead + p.word.raw)
	}
	if len(text) > 0 {
		if len(text) == 1 && text[0].word.raw == "-" { // the text is on stdin
			b.WriteString(text[0].lead + "-")
		} else {
			b.WriteString(text[0].lead + TypedTextMark)
		}
	}
	rest := s[consumed:]
	trimmed := strings.TrimLeft(rest, " \t")
	switch {
	case strings.HasPrefix(trimmed, "<<<"):
		// A here-string gui type reads its text from: the word after it is the text.
		lead := len(rest) - len(trimmed)
		after := strings.TrimLeft(trimmed[3:], " \t")
		w, n := readWord(after)
		gap := len(trimmed) - 3 - len(after)
		b.WriteString(rest[:lead] + "<<<" + strings.Repeat(" ", max(gap, 1)) + TypedTextMark)
		consumed += lead + 3 + gap + n
		typed = append(typed, shellUnquote(w.raw))
		stdin = false
	case strings.HasPrefix(trimmed, "<<"):
		// A here-document gui type reads its text from: its body is masked, its markers kept.
		body, n, lines := maskHeredoc(rest)
		b.WriteString(body)
		consumed += n
		typed = append(typed, lines...)
		stdin = false
	case stdin && strings.HasPrefix(trimmed, "<"):
		stdin = false // a file: its name is not the text
	}
	return typeArgs{masked: b.String(), consumed: consumed, typed: typed, stdin: stdin}
}

// hasValueFlag reports whether pieces hold --role, --name or --see.
func hasValueFlag(pieces []piece) bool {
	for _, p := range pieces {
		if typeValueFlags[p.word.raw] {
			return true
		}
	}
	return false
}

// typeHeredocRe is a here-document's operator and delimiter word.
var typeHeredocRe = regexp.MustCompile(`^([ \t]*<<-?[ \t]*)(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)`)

// maskHeredoc is a here-document at the start of s (its "<<DELIM" first) with its body shown as TypedTextMark, and the
// bytes it covers: through the delimiter's own line, or to the end when it never closes.
func maskHeredoc(s string) (string, int, []string) {
	m := typeHeredocRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, nil
	}
	head := m[0]
	delim := m[3]
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s, len(s), nil // the operator and no body yet
	}
	lineRest := s[len(head):nl] // anything after the delimiter on its line (a pipe, a flag) is kept
	body := s[nl+1:]
	for off := 0; off <= len(body); {
		end := strings.IndexByte(body[off:], '\n')
		line := body[off:]
		if end >= 0 {
			line = body[off : off+end]
		}
		if strings.TrimLeft(line, "\t") == delim {
			return head + lineRest + "\n" + TypedTextMark + "\n" + line, nl + 1 + off + len(line), bodyLines(body[:off])
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	return head + lineRest + "\n" + TypedTextMark, len(s), bodyLines(body)
}

// bodyLines is a here-document body's non-blank lines, and the body whole.
func bodyLines(body string) []string {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return nil
	}
	out := []string{body}
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" && l != body {
			out = append(out, l)
		}
	}
	return out
}

// TypedTexts is what every gui type command in command types, as the shell would hand it over (quotes undone): the
// whole text and each of its words, a here-document's body and a here-string, and the arguments of the commands
// piped into one that reads stdin.
func TypedTexts(command string) []string {
	var out []string
	for _, g := range findGuiTypes(command) {
		out = append(out, g.typed...)
		out = append(out, g.fed...)
	}
	return out
}

// shellUnquote is a shell word with its quoting undone: '...' as is, "..." and bare text with backslash escapes undone.
func shellUnquote(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case '\'':
			j := strings.IndexByte(raw[i+1:], '\'')
			if j < 0 {
				b.WriteString(raw[i+1:])
				return b.String()
			}
			b.WriteString(raw[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; i < len(raw) && raw[i] != '"'; i++ {
				if raw[i] == '\\' && i+1 < len(raw) && strings.IndexByte("\\\"$`", raw[i+1]) >= 0 {
					i++
				}
				b.WriteByte(raw[i])
			}
		case '\\':
			if i+1 < len(raw) {
				i++
				b.WriteByte(raw[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
