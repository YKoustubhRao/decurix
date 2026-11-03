package skills

import (
	"path"
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

// writeTargetRe is a file a command writes: a redirection's target (> f, >> f) or tee's.
var writeTargetRe = regexp.MustCompile(`(?:^|[^>&0-9<])>>?[ \t]*['"]?([^\s'";&|<>()]+)|\btee[ \t]+(?:-a[ \t]+)?['"]?([^\s'";&|<>()]+)`)

// maxWroteFiles bounds the files a goal is remembered to have written.
const maxWroteFiles = 50

// noteWroteFiles keeps the files a command that worked wrote by redirection or tee: a script the work writes there and
// runs later is its own code, as inline code is (review N-W3-8).
func (st *operatorState) noteWroteFiles(command string) {
	for _, m := range writeTargetRe.FindAllStringSubmatch(command, -1) {
		f := m[1] + m[2]
		if f == "" || f == "/dev/null" || strings.HasPrefix(f, "&") || contains(st.WroteFiles, f) || len(st.WroteFiles) >= maxWroteFiles {
			continue
		}
		st.WroteFiles = append(st.WroteFiles, f)
	}
}

// ownCode reports whether a command runs code the work wrote itself: inline code, or a file this goal wrote, named by
// the same path or the same final part of it.
func (st *operatorState) ownCode(command string) bool {
	if inlineCodeRe.MatchString(command) {
		return true
	}
	for _, word := range strings.Fields(command) {
		word = strings.Trim(word, `'"`)
		for _, f := range st.WroteFiles {
			if word == f || path.Base(word) == path.Base(f) && strings.Contains(path.Base(f), ".") {
				return true
			}
		}
	}
	return false
}

// noteComputed keeps the values the command's output holds that nothing seen before held, when the command runs code
// the work wrote itself (ownCode).
func (st *operatorState) noteComputed(command, shown string) {
	if !st.ownCode(command) {
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
