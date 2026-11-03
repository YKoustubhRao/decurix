package llmrec

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// How a held value is found in text, the same rule on the Python side (handoff-CP H-CP-9, shared vectors in
// testdata/held-values.json):
//
//   - a value of minHeldSubstring characters (code points) or more is withheld wherever it occurs, as a substring;
//   - a shorter value is withheld only as a whole token: where the characters right before and after it are not a
//     letter or a digit (or are the start or end of the text). A whole field that equals it is such a token.
//
// So a held "ab9" never cuts "9fab9c" inside a hash, and a held "Zq9X-77" is withheld inside anything.
const minHeldSubstring = 4

// ReplaceHeld is text with every one of values withheld by that rule, longest first.
func ReplaceHeld(text string, values []string) string {
	return replaceForms(text, longestFirst(values))
}

// replaceForms is s with each of forms (longest first) replaced by withhold.Marker, by the rule above.
func replaceForms(s string, forms []string) string {
	for _, f := range forms {
		if f == "" {
			continue
		}
		if utf8.RuneCountInString(f) >= minHeldSubstring {
			s = strings.ReplaceAll(s, f, withhold.Marker)
			continue
		}
		s = replaceTokens(s, f)
	}
	return s
}

// replaceTokens replaces f in s only where it stands as a whole token.
func replaceTokens(s, f string) string {
	var b strings.Builder
	for {
		i := indexToken(s, f)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(withhold.Marker)
		s = s[i+len(f):]
	}
}

// indexToken is the first index of f in s where it stands as a whole token, or -1.
func indexToken(s, f string) int {
	from := 0
	for from <= len(s) {
		j := strings.Index(s[from:], f)
		if j < 0 {
			return -1
		}
		i := from + j
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[i+len(f):])
		if (i == 0 || !wordRune(before)) && (i+len(f) == len(s) || !wordRune(after)) {
			return i
		}
		from = i + 1
	}
	return -1
}

// wordRune reports whether r is a letter or a digit: what joins a short value to a longer word.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// containsHeld reports whether text holds v by the rule above.
func containsHeld(text, v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	if utf8.RuneCountInString(v) >= minHeldSubstring {
		return strings.Contains(text, v)
	}
	return indexToken(text, v) >= 0
}

// longestFirst is values, longest first, empty ones dropped.
func longestFirst(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// jsonStringTokens is the spans [start, end) of every JSON string token in s, quotes included: from an unescaped
// quote to the next unescaped quote.
func jsonStringTokens(s string) [][2]int {
	var spans [][2]int
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] != '"' || escaped(s, i) {
			continue
		}
		if start < 0 {
			start = i
			continue
		}
		spans = append(spans, [2]int{start, i + 1})
		start = -1
	}
	return spans
}

// escaped reports whether the byte at i is preceded by an odd number of backslashes.
func escaped(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// heldUnderEscapes reports whether text, with JSON escapes undone up to three layers, holds one of values.
func heldUnderEscapes(text string, values []string) bool {
	layer := text
	for i := 0; i < 3; i++ {
		next := unescapeJSON(layer)
		if next == layer {
			return false
		}
		for _, v := range values {
			if containsHeld(next, v) {
				return true
			}
		}
		layer = next
	}
	return false
}
