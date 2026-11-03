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

// noteWroteFiles keeps the files a command that worked wrote from its own text: echo or printf redirected into a file,
// or a here-document fed to cat or tee. A script the work writes that way and runs later is its own code, as inline
// code is (review N-W3-8). A file written from anything else - a download, another program's output - is not: its
// content is not the work's (N-W3-9).
func (st *operatorState) noteWroteFiles(command string) {
	for _, seg := range shellSegments(command) {
		words := strings.Fields(seg.Text)
		if len(words) == 0 {
			continue
		}
		prog := path.Base(strings.Trim(words[0], `'"`))
		if !(prog == "echo" || prog == "printf" || ((prog == "cat" || prog == "tee") && seg.HasStdin)) {
			continue
		}
		for _, m := range writeTargetRe.FindAllStringSubmatch(seg.Text, -1) {
			f := m[1] + m[2]
			if f == "" || f == "/dev/null" || strings.HasPrefix(f, "&") || contains(st.WroteFiles, f) || len(st.WroteFiles) >= maxWroteFiles {
				continue
			}
			st.WroteFiles = append(st.WroteFiles, f)
		}
	}
}

// scriptRunners run the file named as their first argument as a program.
var scriptRunners = regexp.MustCompile(`^(sh|bash|zsh|dash|python[0-9.]*|node|nodejs|perl|ruby|php)$`)

// ownCode reports whether a command runs code the work wrote itself: inline code, or a file this goal wrote from its own
// text (noteWroteFiles) that is the program run - executed directly, or the script an interpreter is given. A file
// that is only a word of the command (cat data.csv) is not run (N-W3-9).
func (st *operatorState) ownCode(command string) bool {
	if inlineCodeRe.MatchString(command) {
		return true
	}
	if len(st.WroteFiles) == 0 {
		return false
	}
	for _, seg := range shellSegments(command) {
		words := strings.Fields(seg.Text)
		for len(words) > 0 && strings.Contains(words[0], "=") && !strings.ContainsAny(words[0], "/'\"") {
			words = words[1:] // VAR=value before the program
		}
		if len(words) == 0 {
			continue
		}
		program := strings.Trim(words[0], `'"`)
		if st.wroteFile(program) {
			return true
		}
		if scriptRunners.MatchString(path.Base(program)) {
			for _, w := range words[1:] {
				if strings.HasPrefix(w, "-") {
					continue
				}
				if st.wroteFile(strings.Trim(w, `'"`)) {
					return true
				}
				break
			}
		}
	}
	return false
}

// wroteFile reports whether word names a file this goal wrote from its own text: the same path, or the same final part
// of it (a relative and an absolute name of one file).
func (st *operatorState) wroteFile(word string) bool {
	for _, f := range st.WroteFiles {
		if word == f || (path.Base(word) == path.Base(f) && strings.Contains(path.Base(f), ".")) {
			return true
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
