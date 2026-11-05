package withhold

import (
	"regexp"
	"strings"
)

// Values is what withholding removed from original to make withheld: the values a Marker stands in for, in order.
// withheld must be original with some spans replaced by Marker (what Text and Reply make); a whole text withheld as
// Secret gives the whole original as its one value. Anything that cannot be matched span by span gives the whole
// original too, the safe side. The call recorder learns these values, so a model that echoes one bare is withheld
// as well, and no ordinary word around a value (a "password" label) is ever taken for a secret.
func Values(original, withheld string) []string {
	if original == withheld {
		return nil
	}
	if withheld == Secret || !strings.Contains(withheld, Marker) {
		return wholeValue(original)
	}
	parts := strings.Split(withheld, Marker)
	var pattern strings.Builder
	pattern.WriteString(`(?s)^`)
	for i, p := range parts {
		if i > 0 {
			pattern.WriteString(`(.+?)`)
		}
		pattern.WriteString(regexp.QuoteMeta(p))
	}
	pattern.WriteString(`$`)
	re, err := regexp.Compile(pattern.String())
	if err != nil {
		return wholeValue(original)
	}
	m := re.FindStringSubmatch(original)
	if m == nil {
		return wholeValue(original)
	}
	var out []string
	for _, v := range m[1:] {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// wholeValue is the whole text as one value: as typed (trimmed), and with its spaces folded when that differs.
func wholeValue(text string) []string {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil
	}
	out := []string{t}
	if folded := strings.Join(strings.Fields(t), " "); folded != t {
		out = append(out, folded)
	}
	return out
}

// HeldValues is every value withheld from original in its stored form (Values), and, when stored is the whole text
// withheld (Secret), the values Text would withhold of it too: what the call recorder and an approval gate are told.
func HeldValues(original, stored string) []string {
	values := Values(original, stored)
	if stored == Secret {
		values = append(values, Values(original, Text(original))...)
	}
	return values
}
