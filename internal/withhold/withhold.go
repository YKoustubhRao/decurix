// Package withhold keeps secrets a person typed out of what the control plane stores and shows. A request such as
// "log in with password Xy7#q" goes to the desktop as it was written (the worker needs it), but the copy kept in the
// task records and the chat is the one with the value withheld, so it is never shown in a trace, a report, an export
// or a backup (review 2026-11-02: the task store kept passwords verbatim). The desktop's agent withholds what it
// sends back on its own side (agent privacy.py).
package withhold

import (
	"regexp"
	"strings"
	"unicode"
)

// Marker replaces a withheld value.
const Marker = "[withheld]"

// labelled is a secret's label, the separator after it, and its value.
var labelled = regexp.MustCompile(`(?i)\b(passwords?|passwd|pwd|passcodes?|pass\s+codes?|m-?pins?|pins?|otps?|one[- ]time\s+(?:password|code)|cvv|cvc|api[ _-]?keys?|secret(?:\s+key)?|client[ _-]?secret|access[ _-]?tokens?|auth[ _-]?tokens?|bearer)(\s*(?:is|was|=|:|-)\s*|\s+)(["']?)([^\s"',;]+)`)

// Text is text with the value of every labelled secret replaced by Marker. A value after ":" or "=" is always a
// value; after "is" or a space it is withheld only when it holds a digit or a mark, so "the password is incorrect"
// stays as it is. A postal "PIN code" is not a secret.
func Text(text string) string {
	if text == "" {
		return text
	}
	return labelled.ReplaceAllStringFunc(text, func(match string) string {
		m := labelled.FindStringSubmatch(match)
		label, sep, quote, value := m[1], m[2], m[3], m[4]
		if value == Marker {
			return match
		}
		lowerLabel := strings.ToLower(label)
		if strings.HasPrefix(lowerLabel, "pin") && strings.HasPrefix(strings.ToLower(value), "code") {
			return match
		}
		explicit := strings.ContainsAny(sep, ":=")
		if !explicit && !hasDigitOrMark(value) {
			return match
		}
		return label + sep + quote + Marker
	})
}

// hasDigitOrMark reports whether s holds a digit, or a character that is neither a letter nor a digit.
func hasDigitOrMark(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) || !unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
