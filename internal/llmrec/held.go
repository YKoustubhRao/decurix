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
//   - a value of minHeldSubstring characters (Unicode code points) or more is withheld wherever it occurs, as a
//     substring;
//   - a shorter value is withheld only as a whole token: where the code point right before it and the one right after
//     it are not in Unicode general category L* (a letter) or Nd (a decimal digit), or are the start or end of the
//     text. A whole field that equals it is such a token. ("²", "½" and "Ⅵ" are No/Nl: boundaries.)
//   - values are tried longest first, by code points (ties in the order given), in one pass over the original text:
//     an occurrence that overlaps one already chosen, or a [withheld] marker already in the text, is not withheld
//     again, so no value ever cuts into a marker.
//
// So a held "ab9" never cuts "9fab9c" inside a hash, and a held "Zq9X-77" is withheld inside anything.
const minHeldSubstring = 4

// ReplaceHeld is text with every one of values withheld by that rule, longest first.
func ReplaceHeld(text string, values []string) string {
	return replaceForms(text, longestFirst(values))
}

// replaceForms is s with each of forms (longest first) replaced by withhold.Marker, by the rule above, in one pass
// over s: every occurrence is found in the original text, and one that overlaps a chosen one or a marker is skipped.
func replaceForms(s string, forms []string) string {
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
		j := strings.Index(s[i:], withhold.Marker)
		if j < 0 {
			break
		}
		taken = append(taken, span{i + j, i + j + len(withhold.Marker)})
		i += j + len(withhold.Marker)
	}
	markers := len(taken)
	for _, f := range forms {
		if f == "" {
			continue
		}
		short := utf8.RuneCountInString(f) < minHeldSubstring
		for from := 0; from <= len(s); {
			j := strings.Index(s[from:], f)
			if j < 0 {
				break
			}
			i := from + j
			from = i + 1
			if short && !standsAlone(s, i, len(f)) || overlaps(i, i+len(f)) {
				continue
			}
			taken = append(taken, span{i, i + len(f)})
			from = i + len(f)
		}
	}
	if len(taken) == markers {
		return s
	}
	chosen := taken[markers:]
	for a := 1; a < len(chosen); a++ {
		for b := a; b > 0 && chosen[b].from < chosen[b-1].from; b-- {
			chosen[b], chosen[b-1] = chosen[b-1], chosen[b]
		}
	}
	var out strings.Builder
	last := 0
	for _, c := range chosen {
		out.WriteString(s[last:c.from])
		out.WriteString(withhold.Marker)
		last = c.to
	}
	out.WriteString(s[last:])
	return out.String()
}

// standsAlone reports whether the n bytes at i in s stand as a whole token (the rule above).
func standsAlone(s string, i, n int) bool {
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[i+n:])
	return (i == 0 || !wordRune(before)) && (i+n == len(s) || !wordRune(after))
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
		if standsAlone(s, i, len(f)) {
			return i
		}
		from = i + 1
	}
	return -1
}

// wordRune reports whether r is a letter (L*) or a decimal digit (Nd): what joins a short value to a longer word.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.Is(unicode.Nd, r) }

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
	sortByCodePoints(out)
	return out
}

// sortByCodePoints orders values longest first by code points, ties keeping their order.
func sortByCodePoints(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && utf8.RuneCountInString(values[j]) > utf8.RuneCountInString(values[j-1]); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
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
