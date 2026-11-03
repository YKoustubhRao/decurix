package skills

import (
	"regexp"
	"strings"
)

// A value is traced when any command printed it (grounding.go), so code the work wrote itself can print a value
// nothing gave: python3 -c "print(12340+5)" makes 12345 "seen" (review T-31). That is how working figures are made,
// so it is not refused; the reviewer is told which typed values only such code produced, as a fact with no verdict.

// inlineCodeRe is an interpreter run on code written in the command itself: -c/-e, or a program read from stdin.
var inlineCodeRe = regexp.MustCompile(`(?i)\b(python[0-9.]*|node|nodejs|perl|ruby|php)\b[^\n|;&]*?(\s-[a-zA-Z]*[ce]\b|\s-\s*<<|<<)`)

// Bounds on what is kept.
const (
	maxComputedValues = 400
	maxComputedTyped  = 20
)

// noteComputed keeps the values the command's output holds that nothing seen before held, when the command runs code
// it wrote itself.
func (st *operatorState) noteComputed(command, shown string) {
	if !inlineCodeRe.MatchString(command) {
		return
	}
	for _, v := range valueTokenRe.FindAllString(shown, -1) {
		v = strings.Trim(v, ".,/-_")
		if v == "" || holdsToken(st.Seen, v) {
			continue
		}
		if st.ComputedOnly == nil {
			st.ComputedOnly = map[string]bool{}
		}
		if len(st.ComputedOnly) >= maxComputedValues {
			return
		}
		st.ComputedOnly[v] = true
	}
}

// noteComputedTyped records which typed values only code the work wrote produced.
func (st *operatorState) noteComputedTyped(values []string) {
	for _, v := range values {
		if st.ComputedOnly[strings.Trim(v, ".,/-_")] && !contains(st.ComputedTyped, v) && len(st.ComputedTyped) < maxComputedTyped {
			st.ComputedTyped = append(st.ComputedTyped, v)
		}
	}
}
