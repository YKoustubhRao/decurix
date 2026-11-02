package memory

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

// ErrSecret is why Store refused a fact: it holds a secret-shaped value.
var ErrSecret = errors.New("l1: not stored - it holds a password, token, code or key")

// ErrUnscoped is why a judged fact was not stored: it carries a value the
// person never said, one only the work's documents or output showed.
var ErrUnscoped = errors.New("l1: not stored - it carries a value only seen in this task's work, not said by the person")

var (
	// secretNamedRe is a secret named and then given: "password Northwind#2026",
	// "PIN: 4821", "the OTP is 552910", "api key = abc...". The same reading as
	// the operator's requestSecretRe and decurix_agent/computer/privacy.py.
	secretNamedRe = regexp.MustCompile(`(?i)\b(?:pass(?:word|code|phrase)|pwd|pin|otp|one[- ]time (?:code|password|pin)|` +
		`(?:verification|security|auth(?:entication)?|login|2fa|mfa) code|token|api[ _-]?key|access[ _-]?key|` +
		`secret(?:[ _-]?key)?|private[ _-]?key|client[ _-]?secret|credentials?)\b(?:'s)?` +
		`\s*(is|was|are|=|:)?\s*["'\x{201c}\x{2018}]?([^\s"'\x{201d}\x{2019},;)\]]{3,200})`)
	// tokenShapeRe is a credential recognisable by its shape alone.
	tokenShapeRe = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|` +
		`\bxox[abprs]-[A-Za-z0-9-]{10,}|\bAKIA[0-9A-Z]{16}\b|\bAIza[0-9A-Za-z_-]{35}|` +
		`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|` +
		`(?i:\bbearer\s+[A-Za-z0-9._~+/=-]{16,})|://[^/\s:@]+:[^/\s@]+@`)
	// longRunRe is a long unbroken run of key characters, tested for mixed case and digits.
	longRunRe = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// notAValue are words that follow "password is" without being one ("the API key is stored in the vault").
var notAValue = map[string]bool{
	"stored": true, "kept": true, "saved": true, "in": true, "on": true, "at": true, "the": true, "a": true, "an": true,
	"not": true, "required": true, "needed": true, "missing": true, "set": true, "expired": true, "valid": true,
	"invalid": true, "wrong": true, "correct": true, "changed": true, "rotated": true, "available": true,
	"unknown": true, "private": true, "confidential": true, "shared": true, "sent": true, "with": true, "for": true,
	"from": true, "to": true, "managed": true, "handled": true, "never": true, "always": true, "only": true,
}

// LooksSecret reports whether text holds a secret-shaped value: a password,
// PIN, OTP or other code given after its name, a token or key recognisable by
// its shape, credentials in a link, or a long random-looking string. It errs
// towards yes: a fact refused costs a reminder, a secret kept is a leak.
func LooksSecret(text string) bool {
	if tokenShapeRe.MatchString(text) {
		return true
	}
	for _, m := range secretNamedRe.FindAllStringSubmatch(text, -1) {
		sep, value := m[1], strings.TrimRight(m[2], ".!?:")
		if len(value) < 3 {
			continue
		}
		if sep != "" {
			if !notAValue[strings.ToLower(value)] {
				return true
			}
			continue
		}
		// "password manager", "token budget": a plain word after the name is
		// not a value; one with a digit or a symbol is ("password Northwind#2026").
		if strings.IndexFunc(value, func(r rune) bool { return !unicode.IsLetter(r) }) >= 0 {
			return true
		}
	}
	for _, run := range longRunRe.FindAllString(text, -1) {
		var upper, lower, digit bool
		for _, r := range run {
			switch {
			case unicode.IsUpper(r):
				upper = true
			case unicode.IsLower(r):
				lower = true
			case unicode.IsDigit(r):
				digit = true
			}
		}
		if upper && lower && digit {
			return true
		}
	}
	return false
}

// valueRe is a value-shaped token of a fact: anything with a digit in it
// (an amount, an identifier, a phone number, a date) or an email address.
var valueRe = regexp.MustCompile(`[\p{L}\p{N}._%+-]+@[\p{L}\p{N}.-]+\.[\p{L}]{2,}|[\p{L}\p{N}._/#+-]*\p{N}[\p{L}\p{N}._/#+-]*`)

// unsaidValues are the value-shaped tokens of fact that said does not hold:
// values a judged fact took from the worker's answer - from a client's
// documents, a page, a command's output - rather than from the person.
func unsaidValues(fact, said string) []string {
	lowerSaid := strings.ToLower(said)
	squashed := squash(lowerSaid)
	var out []string
	for _, v := range valueRe.FindAllString(fact, -1) {
		v = strings.Trim(v, ".,-/#")
		if v == "" {
			continue
		}
		lv := strings.ToLower(v)
		if strings.Contains(lowerSaid, lv) || strings.Contains(squashed, squash(lv)) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// squash drops the separators people write values with ("98 450-12345" is "9845012345").
func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '.' || r == ',' || r == '(' || r == ')' || r == '/' {
			return -1
		}
		return r
	}, s)
}
