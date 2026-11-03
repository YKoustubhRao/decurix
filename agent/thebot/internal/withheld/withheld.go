// Package withheld carries a job's secret values (PLAN-P2 C14): the values the person marked secret, or the control
// plane withheld, told to soul as data on the job ("secrets" on /api/chat), never in the prompt. The model is given
// the request as it is, so the work can be done; nothing soul writes down, logs into a job, serves or stores keeps a
// copy (Text, Holds, Whole).
//
// Values ride on the job's context (With, From). They are never persisted.
package withheld

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Mark is what a secret value is replaced with: the control plane's and the agent's marker too
// (internal/withhold.Marker, privacy._MARKER), so a marker already in a text is known whichever side wrote it.
const Mark = "[withheld]"

// MinHeldSubstring is the shortest value withheld wherever it occurs, as a substring; a shorter value is withheld
// only as a whole token (the held-value rule shared with the control plane and the agent,
// internal/llmrec/testdata/held-values.json).
const MinHeldSubstring = 4

// MaxValues bounds the values one job may be told: a job sent more is refused (soul's /api/chat), never cut, so a
// job's own value is never dropped to make room.
const MaxValues = 1000

// MaxValueLen bounds one value a job may be told.
const MaxValueLen = 64 << 10

type key struct{}

// Values is a job's secret values, in the order given (Text tries them longest first).
type Values []string

// Clean is values trimmed, without blanks or repeats, in the order given. It never drops a value for room: the caller
// bounds what it accepts (MaxValues, MaxValueLen). A short value is safe to keep: it is withheld only as a whole token.
func Clean(values []string) Values {
	seen := map[string]bool{}
	var out Values
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// With returns ctx carrying values (cleaned) on top of any it already carries.
func With(ctx context.Context, values []string) context.Context {
	all := append(append([]string{}, From(ctx)...), values...)
	v := Clean(all)
	if len(v) == 0 {
		return ctx
	}
	return context.WithValue(ctx, key{}, v)
}

// From is the secret values ctx carries.
func From(ctx context.Context) Values {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(key{}).(Values)
	return v
}

// Text is text with every one of values withheld, in each of its written forms (WrittenForms: as a shell, JSON or
// a URL writes it, so a quoted step line is caught), by the held-value rule shared with the control plane and the
// agent (internal/llmrec/testdata/held-values.json):
//   - a form of MinHeldSubstring or more code points is withheld wherever it occurs, as a substring;
//   - a shorter one only as a whole token: the code point before it and the one after it are each not a letter (L*)
//     or a decimal digit (Nd), or are the start or end of the text;
//   - forms are tried longest first by code points (ties in the order given), in one pass over the original text: an
//     occurrence overlapping one already chosen, or a marker already in the text, is skipped.
//
// Each occurrence becomes Mark.
func (v Values) Text(text string) string {
	if len(v) == 0 || text == "" {
		return text
	}
	return replaceHeld(text, v.forms())
}

// forms is every written form of every value, longest first by code points, ties in the order given.
func (v Values) forms() []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range v {
		for _, f := range WrittenForms(value) {
			if f != "" && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return utf8.RuneCountInString(out[i]) > utf8.RuneCountInString(out[j]) })
	return out
}

// WrittenForms is the forms value takes in a command or a URL, as the agent's approval.written_forms gives them, in
// the same order: as it is; shlex.quote; inside double quotes (\ " $ ` escaped); inside single quotes (each quote
// written as the four characters quote, backslash, quote, quote); JSON-escaped (non-ASCII kept, then \u-escaped);
// percent-encoded (quote with safe="", quote_plus, quote).
func WrittenForms(value string) []string {
	if value == "" {
		return nil
	}
	var double strings.Builder
	for _, r := range value {
		if strings.ContainsRune("\\\"$`", r) {
			double.WriteByte('\\')
		}
		double.WriteRune(r)
	}
	forms := []string{value, shellQuote(value), double.String(), strings.ReplaceAll(value, "'", `'\''`),
		jsonEscape(value, false), jsonEscape(value, true), percent(value, "", false), percent(value, "", true),
		percent(value, "/", false)}
	seen := map[string]bool{}
	var out []string
	for _, f := range forms {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// shellQuote is Python's shlex.quote: value as is when every byte is of [A-Za-z0-9_@%+=:,./-], else single-quoted
// with each quote written '"'"'.
func shellQuote(value string) string {
	safe := true
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_@%+=:,./-", c) >= 0) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// jsonEscape is Python's json.dumps(value)[1:-1]: quote and backslash escaped, \n \r \t \b \f, other control
// characters as \u00xx; with ascii, every non-ASCII character as \uxxxx (a surrogate pair past U+FFFF).
func jsonEscape(value string, ascii bool) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			case ascii && r > 0x7f && r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			case ascii && r > 0xffff:
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// percent is Python's urllib.parse.quote(value, safe) (plus: quote_plus, where a space is +): every UTF-8 byte but
// A-Za-z0-9 _ . - ~ and safe written %XX.
func percent(value, safe string, plus bool) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("_.-~", c) >= 0,
			strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		case plus && c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// replaceHeld is text with each of forms (longest first) withheld by the held-value rule (Text), in one pass over
// the original text.
func replaceHeld(text string, forms []string) string {
	type span struct{ from, to int }
	var taken []span
	overlaps := func(from, to int) bool {
		for _, t := range taken {
			if from < t.to && t.from < to {
				return true
			}
		}
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], Mark)
		if j < 0 {
			break
		}
		taken = append(taken, span{i + j, i + j + len(Mark)})
		i += j + len(Mark)
	}
	markers := len(taken)
	for _, f := range forms {
		short := utf8.RuneCountInString(f) < MinHeldSubstring
		for from := 0; from <= len(text); {
			j := strings.Index(text[from:], f)
			if j < 0 {
				break
			}
			i := from + j
			from = i + 1
			if short && !standsAlone(text, i, len(f)) || overlaps(i, i+len(f)) {
				continue
			}
			taken = append(taken, span{i, i + len(f)})
			from = i + len(f)
		}
	}
	chosen := taken[markers:]
	if len(chosen) == 0 {
		return text
	}
	sort.Slice(chosen, func(a, b int) bool { return chosen[a].from < chosen[b].from })
	var out strings.Builder
	last := 0
	for _, c := range chosen {
		out.WriteString(text[last:c.from])
		out.WriteString(Mark)
		last = c.to
	}
	out.WriteString(text[last:])
	return out.String()
}

// standsAlone reports whether the n bytes at i in s stand as a whole token: no letter or decimal digit touches them.
func standsAlone(s string, i, n int) bool {
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[i+n:])
	return (i == 0 || !wordRune(before)) && (i+n == len(s) || !wordRune(after))
}

// wordRune reports whether r is a letter (L*) or a decimal digit (Nd): what joins a short value to a longer word.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.Is(unicode.Nd, r) }

// Shown is text as it may leave soul: Mark when the whole of it is secret, else Text. Withhold first, then shorten
// or quote: a value cut or escaped before it is withheld no longer matches, and its part leaks.
func (v Values) Shown(text string) string {
	if v.Whole(text) {
		return Mark
	}
	return v.Text(text)
}

// Holds reports whether text holds any of values, in any written form, by the held-value rule (Text).
func (v Values) Holds(text string) bool {
	return len(v) > 0 && v.Text(text) != text
}

// Whole reports whether text, trimmed, is one of values: the whole request was secret, so none of it is kept.
func (v Values) Whole(text string) bool {
	t := strings.TrimSpace(text)
	for _, s := range v {
		if t == s {
			return true
		}
	}
	return false
}
