// Package verify checks an answer against what it was made from, in code,
// before anyone trusts it. It knows no domain: it looks only for things any
// answer can be held to, such as a figure that appears in no source.
//
// Its findings are advisory. A number an answer computed (a total of two read
// figures) appears in no source and is still right, so the check never
// changes an answer; it lists what a person or a stronger model should look at.
//
// Depends on nothing internal.
package verify

import (
	"regexp"
	"sort"
	"strings"
)

// numberRe finds figures: 12,345.00 / 4417 / 3.5 / 1,20,000 (Indian grouping).
var numberRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// normal strips grouping commas and a trailing ".0..." so 12,345.00 and 12345 match.
func normal(n string) string {
	n = strings.ReplaceAll(n, ",", "")
	if i := strings.IndexByte(n, '.'); i >= 0 {
		frac := strings.TrimRight(n[i+1:], "0")
		if frac == "" {
			n = n[:i]
		} else {
			n = n[:i] + "." + frac
		}
	}
	return strings.TrimLeft(n, "0")
}

// Figures returns the normalised figures of two or more digits in text.
// Single digits are list numbers and counts far more often than read values.
func Figures(text string) []string {
	var out []string
	for _, m := range numberRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ",.")
		digits := strings.Count(m, "") - 1 - strings.Count(m, ",") - strings.Count(m, ".")
		if digits < 2 {
			continue
		}
		if n := normal(m); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// UnsupportedFigures lists the figures in answer that no source contains.
func UnsupportedFigures(answer string, sources ...string) []string {
	have := map[string]bool{}
	for _, s := range sources {
		for _, f := range Figures(s) {
			have[f] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range Figures(answer) {
		if !have[f] && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
