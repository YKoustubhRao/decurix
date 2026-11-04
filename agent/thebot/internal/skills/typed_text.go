package skills

import (
	"regexp"
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
// as one does not leave soul in them. The target stays (an element ref, a cell, a role and name, or --role/--name/
// --see), and so do the flags; a here-document's body that gui type reads its text from is masked too. The command
// that runs is never this text: only what soul shows of it.
func MaskTypedText(s string) string {
	if !strings.Contains(s, "gui") {
		return s
	}
	var out strings.Builder
	rest := s
	for {
		loc := guiTypeRe.FindStringIndex(rest)
		if loc == nil {
			out.WriteString(rest)
			return out.String()
		}
		at := loc[1] // just past "type" and the blank after it (or the end)
		out.WriteString(rest[:at])
		masked, consumed, _ := maskTypeArgs(rest[at:])
		out.WriteString(masked)
		rest = rest[at+consumed:]
	}
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

// maskTypeArgs masks the text of one gui type's arguments at the start of s, and returns what it wrote and how many
// bytes of s that covers (up to the command's end: a separator, a newline, or past a here-document it reads).
func maskTypeArgs(s string) (string, int, []piece) {
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
	if len(text) > 1 && text[0].word.quoted {
		// A quoted text is the whole of it as written: what follows is the line's own (in a sentence quoting the
		// command, the words after it), and is kept.
		consumed, text = text[0].end, text[:1]
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
	// A here-document gui type reads its text from: its body is masked, its markers kept.
	if rest := s[consumed:]; strings.HasPrefix(strings.TrimLeft(rest, " \t"), "<<") {
		body, n := maskHeredoc(rest)
		b.WriteString(body)
		consumed += n
	}
	return b.String(), consumed, text
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
func maskHeredoc(s string) (string, int) {
	m := typeHeredocRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0
	}
	head := m[0]
	delim := m[3]
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s, len(s) // the operator and no body yet
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
			return head + lineRest + "\n" + TypedTextMark + "\n" + line, nl + 1 + off + len(line)
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	return head + lineRest + "\n" + TypedTextMark, len(s)
}

// TypedTexts is what every gui type command in command types, as the shell would hand it over (quotes undone): the
// whole text and each of its words. A here-document's body is not here (it is masked where shown, and gui reads it).
func TypedTexts(command string) []string {
	var out []string
	rest := command
	for {
		loc := guiTypeRe.FindStringIndex(rest)
		if loc == nil {
			return out
		}
		at := loc[1]
		_, consumed, text := maskTypeArgs(rest[at:])
		var words []string
		for _, p := range text {
			if p.word.raw == "-" {
				continue
			}
			words = append(words, shellUnquote(p.word.raw))
		}
		if len(words) > 0 {
			out = append(out, strings.Join(words, " "))
			if len(words) > 1 {
				out = append(out, words...)
			}
		}
		rest = rest[at+consumed:]
	}
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
