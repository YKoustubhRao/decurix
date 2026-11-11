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
	// secretNamedRe is a credential word, an optional separator and the word after it: "password Northwind#2026",
	// "PIN: 4821", "the OTP is 552910", "api key = abc...", "passwd hunter2", "login is Ledger@123". Whether the word
	// after it is a secret VALUE depends on the kind of credential (secretValue): the bare words are not secrets
	// ("unexpected token: foo", "pin numpy<2", "the API key is stored in the vault"). Longer names come first in the
	// alternation so "login code" is read as a code, not as a login.
	secretNamedRe = regexp.MustCompile(`(?i)\b(one[- ]time (?:code|password|pin)|` +
		`(?:verification|security|auth(?:entication)?|login|sign[- ]?in|2fa|mfa) code|` +
		`pass(?:word|wd|code|phrase)s?|pwd|pw|pin|otp|` +
		`api[ _-]?(?:key|token)|access[ _-]?(?:key|token)|auth[ _-]?token|refresh[ _-]?token|secret[ _-]?key|private[ _-]?key|` +
		`client[ _-]?secret|token|login|credentials?|creds|secret)\b(?:'s)?` +
		`(?:\s*(is|was|are|=|:)|\s+(-|\x{2013}))?\s*["'\x{201c}\x{2018}]?([^\s"'\x{201d}\x{2019},;)\]]{3,200})`)
	// tokenShapeRe is a credential recognisable by its shape alone.
	tokenShapeRe = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|` +
		`\bxox[abprs]-[A-Za-z0-9-]{10,}|\bAKIA[0-9A-Z]{16}\b|\bAIza[0-9A-Za-z_-]{35}|` +
		`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|` +
		`(?i:\bbearer\s+[A-Za-z0-9._~+/=-]{16,})|://[^/\s:@]+:[^/\s@]+@`)
	// longRunRe is a long unbroken run of key characters, tested for mixed case and digits.
	longRunRe = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
	// digitsRe is a numeric code: a PIN or a one-time code is digits.
	digitsRe = regexp.MustCompile(`^\d{4,10}$`)
	// keyCharsRe is a value made only of the characters keys and tokens are written with.
	keyCharsRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=:-]+$`)
	// emailOrLinkRe is a value that names an account or a place, not a secret ("login: a@b.example").
	emailOrLinkRe = regexp.MustCompile(`(?i)^([^@\s]+@[^@\s]+\.[a-z]{2,}|[a-z][a-z0-9+.-]*://\S*|www\.\S+)$`)
)

// notAValue are words that follow "password is" without being one ("the API key is stored in the vault").
var notAValue = map[string]bool{
	"stored": true, "kept": true, "saved": true, "in": true, "on": true, "at": true, "the": true, "a": true, "an": true,
	"not": true, "required": true, "needed": true, "missing": true, "set": true, "expired": true, "valid": true,
	"invalid": true, "wrong": true, "correct": true, "changed": true, "rotated": true, "available": true,
	"unknown": true, "private": true, "confidential": true, "shared": true, "sent": true, "with": true, "for": true,
	"from": true, "to": true, "managed": true, "handled": true, "never": true, "always": true, "only": true,
	"same": true, "reset": true, "empty": true, "blank": true, "used": true, "given": true, "updated": true,
}

// credentialKind sorts a credential word by what a secret value of it looks like.
type credentialKind int

const (
	kindPassword credentialKind = iota // a password: any word given as one
	kindLogin                          // a login or a secret: a word with a digit or a symbol, given as one
	kindCode                           // a PIN or a one-time code: digits
	kindKey                            // a token or a key: a long run of key characters with letters and digits
)

// kindOf is the credentialKind of a credential word as secretNamedRe found it.
func kindOf(word string) credentialKind {
	w := strings.ToLower(word)
	switch {
	case strings.HasPrefix(w, "pass") || strings.HasSuffix(w, "password") || w == "pwd" || w == "pw":
		return kindPassword
	case strings.HasSuffix(w, "code") || strings.HasSuffix(w, "pin") || w == "otp":
		return kindCode
	case w == "login" || w == "credential" || w == "credentials" || w == "creds" || w == "secret":
		return kindLogin
	}
	return kindKey
}

// hasDigitOrSymbol reports whether a value has a digit or a symbol a password is written with.
func hasDigitOrSymbol(v string) bool {
	return strings.IndexFunc(v, func(r rune) bool { return unicode.IsDigit(r) || strings.ContainsRune("!@#$%^&*_+=?~|", r) }) >= 0
}

// isReference reports whether a value names where a secret is kept rather than being it: a variable, a
// placeholder, an environment lookup ("password=$DB_PASS", "token: <your token>", "key=os.environ[...]").
func isReference(v string) bool {
	if v == "" || strings.ContainsAny(v[:1], "$<{%[") {
		return true
	}
	lv := strings.ToLower(v)
	return strings.Contains(lv, "environ") || strings.Contains(lv, "getenv") || strings.HasPrefix(lv, "env.")
}

// secretValue reports whether value, found after a credential word of kind (with a separator such as "is" or ":"
// between them, or not), is that credential's value rather than an ordinary word next to its name.
func secretValue(kind credentialKind, separated bool, value string) bool {
	if len(value) < 3 || isReference(value) || notAValue[strings.ToLower(value)] {
		return false
	}
	switch kind {
	case kindPassword:
		// "password manager" is not a value; "password: hunter" and "passwd hunter2" are.
		return separated || hasDigitOrSymbol(value)
	case kindLogin:
		// "login is Ledger@123" is; "login: admin", "login page", "login: a@b.example" are not.
		return separated && hasDigitOrSymbol(value) && !emailOrLinkRe.MatchString(value)
	case kindCode:
		// "PIN: 4821", "otp 552910"; never "pin numpy<2" or "PIN code".
		return digitsRe.MatchString(value)
	default:
		// "api key = abcd1234efgh"; never "unexpected token: foo" or "token budget".
		if !keyCharsRe.MatchString(value) {
			return false
		}
		var letter, digit bool
		for _, r := range value {
			letter = letter || unicode.IsLetter(r)
			digit = digit || unicode.IsDigit(r)
		}
		return (len(value) >= 8 && letter && digit) || len(value) >= 20
	}
}

// namedSecret is the value a secretNamedRe match m in text gives after a credential's name, and whether it is that
// credential's value (secretValue). A plain word after "password:" is withheld even where it reads as the person's
// instruction ("password: ask the client"): a word too many withheld is harmless, and "password: letmein then press
// login" is a password running on into its sentence (review of freeze 2, A1: a running-word exemption let it through).
func namedSecret(text string, m []int) (string, bool) {
	if m[8] < 0 {
		return "", false
	}
	separated := m[4] >= 0 || m[6] >= 0
	value := strings.TrimRight(text[m[8]:m[9]], ".!?:")
	return value, secretValue(kindOf(text[m[2]:m[3]]), separated, value)
}

// LooksSecret reports whether text holds a secret VALUE: a password, PIN,
// OTP, login or key given after its name (secretValue decides, by the kind
// of credential, whether the word after the name is one), a token or key
// recognisable by its shape, credentials in a link, or a long random-looking
// string. The credential words alone are not secrets: a fix that mentions a
// token, a key or pinning a version is kept. For a password it errs towards
// yes: a fact refused costs a reminder, a secret kept is a leak.
func LooksSecret(text string) bool {
	if tokenShapeRe.MatchString(text) {
		return true
	}
	for _, m := range secretNamedRe.FindAllStringSubmatchIndex(text, -1) {
		if _, ok := namedSecret(text, m); ok {
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

// Withheld is what RedactSecrets writes in place of a secret.
const Withheld = "[withheld]"

// RedactSecrets is text with every secret LooksSecret would find replaced by Withheld: a token or key by its shape,
// credentials in a link, the value after a credential's name (decided by kind, as LooksSecret decides it), and a long
// random-looking run. It uses LooksSecret's own rules, so what is withheld and what is judged a secret never drift.
func RedactSecrets(text string) string {
	text = tokenShapeRe.ReplaceAllString(text, Withheld)
	var b strings.Builder
	last := 0
	for _, m := range secretNamedRe.FindAllStringSubmatchIndex(text, -1) {
		value, ok := namedSecret(text, m)
		if !ok {
			continue
		}
		b.WriteString(text[last:m[8]])
		b.WriteString(Withheld)
		last = m[8] + len(value)
	}
	b.WriteString(text[last:])
	text = b.String()
	return longRunRe.ReplaceAllStringFunc(text, func(run string) string {
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
			return Withheld
		}
		return run
	})
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
