// Package withhold keeps secrets a person typed out of what the control plane stores and shows. A request such as
// "log in with password Xy7#q" goes to the desktop as it was written (the worker needs it), but the copy kept in the
// task records and the chat is the one with the value withheld, so it is never shown in a trace, a report, an export
// or a backup (review 2026-11-02: the task store kept passwords verbatim). The desktop's agent withholds what it
// sends back on its own side (agent privacy.py); the rules for a PIN and a card number are the same on both sides.
package withhold

import (
	"regexp"
	"strings"
	"unicode"
)

// Marker replaces a withheld value.
const Marker = "[withheld]"

// Secret is what is kept, everywhere, of a reply to a question asked for a secret, or of anything a person marked as
// a secret when they sent it: the whole text, not a value in it. The desktop still receives it as typed.
//
// That flag - set by the agent from where its question came from, or by the person's own "secret" toggle - is what
// the "secrets never reach the records" invariant rests on. Text below is only a backstop: a word list cannot know
// every way a person writes a password (review 2026-11-03, C-03), so it may grow only where withholding more is
// harmless, never as the fix for a leak.
const Secret = "[secret withheld]"

// labelled is a secret's label, the separator after it, and its value: quoted (all of it, spaces included), or
// running to a space or a quote, so a password with a ";" or "," in it is withheld whole (review 2026-11-03:
// "password: Abc;123xyz" kept ";123xyz", and `password: "my pass 1"` kept "pass 1").
var labelled = regexp.MustCompile(`(?i)\b(passwords?|passwd|pwd|passcodes?|pass\s+codes?|otps?|one[- ]time\s+(?:password|code)|verification\s+code|(?:login|sign[- ]?in|access|auth(?:entication)?)\s+codes?|cvv2?|cvc2?|api[ _-]?keys?|secret(?:\s+key)?|client[ _-]?secret|access[ _-]?tokens?|auth[ _-]?tokens?|token|pass|pw|creds|key)(\s*(?:is|was|=|:|-)\s*|\s+)(?:"([^"\n]{1,200})"|'([^'\n]{1,200})'|([^\s"']+))`)

// pinLabelled is a PIN's label and its value: "UPI PIN 4821", "ATM PIN code: 1234", "PIN: 576104", "PIN-560001". A
// PIN is digits only, four to eight of them (never "Mumbai" in "Pin: Mumbai 400001").
var pinLabelled = regexp.MustCompile(`(?i)\b((?:card|atm|upi|login|debit|credit|bank(?:ing)?|transaction|security|account)[\s_-]*pins?|m-?pins?|t-?pins?|pins?)((?:[\s_-]*(?:code|number|no)\b\.?)?)(\s*(?:is|was)\s+|\s*[:=#-]?\s*)(["']?)(\d{4,8})`)

// pinSecretCue is a word that makes the PIN beside it a sign-in's or a payment's.
var pinSecretCue = regexp.MustCompile(`(?i)\b(?:log\s?in|sign\s?in|user(?:name|\s?id)?|password|passcode|otp|portal|net\s?banking|bank|card|atm|upi|secret|credential|enter|type|my|your|his|her|their|our)\b`)

// pinAddressCue is a word that makes the PIN beside it an address's postal code.
var pinAddressCue = regexp.MustCompile(`(?i)\b(?:address|addr|road|rd|street|st|lane|marg|nagar|colony|sector|layout|city|town|village|district|dist|taluk|tehsil|state|india|ship(?:ping)?\s+to|deliver(?:y|ed)?|billing|bill\s+to|postal|post|zip|floor|building|bldg|near|opp(?:osite)?|landmark|cross|main|east|west|north|south)\b`)

// placeBefore is a place's name right before a PIN's label ("Bengaluru PIN 560001", "Udupi, PIN: 576104").
var placeBefore = regexp.MustCompile(`\b([A-Z][a-z]{2,})\s*[,-]?\s*$`)

// notAPlace are capitalised words that start a sentence rather than name a place ("Your PIN", "Enter PIN").
var notAPlace = map[string]bool{"the": true, "your": true, "my": true, "his": true, "her": true, "their": true,
	"our": true, "its": true, "this": true, "that": true, "new": true, "old": true, "current": true, "default": true,
	"temporary": true, "temp": true, "enter": true, "use": true, "with": true, "and": true, "same": true,
	"secret": true, "login": true, "user": true, "type": true, "send": true, "share": true, "give": true,
	"also": true, "then": true, "please": true, "kindly": true, "card": true, "bank": true, "otp": true,
	"password": true, "portal": true, "account": true, "transaction": true, "security": true, "debit": true,
	"credit": true, "upi": true, "atm": true, "here": true, "set": true, "change": true, "reset": true,
	"valid": true, "wrong": true, "correct": true, "only": true, "is": true, "was": true}

// shapes are credentials that are secret by their shape alone, whatever word is beside them.
var shapes = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{22,}`),
	regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
}

// bearer is "Bearer x" or "Basic x" with a token-like value ("bearer token abc.def.ghi" included).
var bearer = regexp.MustCompile(`(?i)\b(?:bearer|basic)(?:\s+token)?\s*[:=]?\s+([A-Za-z0-9._~+/-]{16,}=*)`)

// urlPassword is the password in https://user:password@host.
var urlPassword = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@`)

// cardNumber is a run of 13-19 digits, maybe split by spaces or dashes, that may be a card's number.
var cardNumber = regexp.MustCompile(`\d(?:[ -]?\d){12,18}`)

// cardGrouped is how a card number is written: fours (Amex 4-6-5) split by spaces or dashes.
var cardGrouped = regexp.MustCompile(`^\d{4}[ -]\d{4}[ -]\d{4}[ -]\d{1,7}$|^\d{4}[ -]\d{6}[ -]\d{4,5}$`)

// cardWord names a card; notCardWord names another identifier a long number often is (one in ten passes the
// card checksum).
var cardWord = regexp.MustCompile(`(?i)\b(?:card|credit|debit|visa|master\s?card|rupay|amex|maestro|cc)\b`)
var notCardWord = regexp.MustCompile(`(?i)\b(?:a/c|account|acct|order|invoice|inv|ref(?:erence)?|utr|awb|tracking|total|amount|phone|mobile|aadhaa?r|uan|policy|folio|ack(?:nowledge?ment)?|irn|txn|transaction|po|bill|voucher|serial|imei)\b`)

// Text is text with every secret it spells out replaced by Marker: a value after a secret's label, a PIN, a card
// number, and a credential known by its shape (an API key, a GitHub token, a JWT, a bearer token, a password in a web
// address). A value after ":" or "=" is always a value; after "is" or a space it is withheld only when it holds a
// digit or a mark ("the password is incorrect" stays), and after a key's or token's name only when it reads as a
// machine's token. A postal code is kept: "PIN code 576104", "Bengaluru PIN 560001"; six digits after a bare "PIN"
// with no address beside them are withheld.
//
// A value held for its owner's life (HoldAlways: a desktop's screen password) is withheld first, wherever it is.
func Text(text string) string {
	if text == "" {
		return text
	}
	text = Known(text)
	for _, re := range shapes {
		text = re.ReplaceAllString(text, Marker)
	}
	text = replaceGroup(text, bearer, 1, func(v string) bool { return tokenish(v) })
	text = replaceGroup(text, urlPassword, 1, func(v string) bool { return v != Marker })
	text = withholdPins(text)
	text = withholdCards(text)
	return withholdLabelled(text)
}

// replaceGroup replaces group g of every match of re whose value secret accepts with Marker.
func replaceGroup(text string, re *regexp.Regexp, g int, secret func(string) bool) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		s, e := m[2*g], m[2*g+1]
		if s < 0 || !secret(text[s:e]) {
			continue
		}
		b.WriteString(text[last:s])
		b.WriteString(Marker)
		last = e
	}
	b.WriteString(text[last:])
	return b.String()
}

// withholdLabelled withholds the value after every secret's label.
func withholdLabelled(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range labelled.FindAllStringSubmatchIndex(text, -1) {
		if m[0] > 0 && isWordByte(text[m[0]-1]) {
			continue
		}
		label, sep := strings.ToLower(text[m[2]:m[3]]), text[m[4]:m[5]]
		quoted := false
		vs, ve := m[10], m[11]
		for g := 3; g <= 4; g++ {
			if m[2*g] >= 0 {
				vs, ve, quoted = m[2*g], m[2*g+1], true
			}
		}
		value := text[vs:ve]
		trimmed := value
		if !quoted {
			// Punctuation that ends the value ("hunter2," "x9y8z7);") is the sentence's, not the secret's.
			trimmed = strings.TrimRight(value, ".,;:!?)]")
		}
		if trimmed == "" || strings.HasPrefix(value, Marker) || !labelledSecret(label, sep, trimmed, quoted) {
			continue
		}
		b.WriteString(text[last:vs])
		b.WriteString(Marker)
		last = vs + len(trimmed)
	}
	b.WriteString(text[last:])
	return b.String()
}

// labelledSecret reports whether value, after label and sep (quoted or not), is a secret's value. A dash is as
// explicit as a colon ("Password - Tiger"): withholding a word too many there is harmless.
func labelledSecret(label, sep, value string, quoted bool) bool {
	explicit := strings.ContainsAny(sep, ":=-")
	if label == "token" {
		// A bare "token" names a queue's or a counter's number as often ("Token: 45", "token number: 7781"): only a
		// machine's token is withheld, or with ":" a value of letters and digits or marks.
		return tokenish(value) || (explicit && len(value) >= 3 && hasLetter(value) && hasDigitOrMark(value))
	}
	if quoted || explicit {
		return true
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") {
		return false // "pwd /home/user/work" is the shell's command and a folder
	}
	switch {
	case strings.HasSuffix(label, "code") || strings.HasSuffix(label, "codes") || strings.HasPrefix(label, "otp"):
		return allDigits(value) && len(value) >= 4 && len(value) <= 8 || len(value) >= 3 && hasDigitOrMark(value)
	case strings.HasPrefix(label, "api") || strings.HasPrefix(label, "secret") || strings.HasPrefix(label, "client") ||
		strings.HasPrefix(label, "access") || strings.HasPrefix(label, "auth"):
		return tokenish(value) // "secret 2024 strategy deck" is a year
	case strings.HasPrefix(label, "cvv") || strings.HasPrefix(label, "cvc"):
		return allDigits(value) && len(value) >= 3 && len(value) <= 4
	}
	return len(value) >= 3 && hasDigitOrMark(value)
}

// hasLetter reports whether s holds a letter.
func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// withholdPins withholds every PIN's value but an address's postal code.
func withholdPins(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range pinLabelled.FindAllStringSubmatchIndex(text, -1) {
		if m[0] > 0 && isWordByte(text[m[0]-1]) {
			continue
		}
		vs, ve := m[10], m[11]
		if ve < len(text) && (isWordByte(text[ve])) {
			continue // "PIN 4821abc" or more digits: not a PIN's value
		}
		if pinKind(text, m[0], ve, text[m[2]:m[3]], text[m[4]:m[5]], text[vs:ve]) == "postal" {
			continue
		}
		b.WriteString(text[last:vs])
		b.WriteString(Marker)
		last = ve
	}
	b.WriteString(text[last:])
	return b.String()
}

// pinKind says what a PIN between start and end in text is: "secret", "postal" or "unsure" (agent privacy.pin_kind).
func pinKind(text string, start, end int, label, suffix, value string) string {
	l := strings.ToLower(strings.TrimRight(label, "sS"))
	if l != "pin" {
		return "secret"
	}
	if strings.ToLower(strings.Trim(suffix, " _-.")) == "code" {
		return "postal"
	}
	if len(value) != 6 || value[0] == '0' {
		return "secret" // an Indian postal code is exactly six digits, the first 1-9
	}
	lineStart := strings.LastIndexByte(text[:start], '\n') + 1
	if start-60 > lineStart {
		lineStart = start - 60
	}
	lineEnd := len(text)
	if i := strings.IndexByte(text[end:], '\n'); i >= 0 {
		lineEnd = end + i
	}
	if end+30 < lineEnd {
		lineEnd = end + 30
	}
	before, after := text[lineStart:start], text[end:lineEnd]
	place := false
	if m := placeBefore.FindStringSubmatch(before); m != nil && !notAPlace[strings.ToLower(m[1])] {
		place = true
	}
	address := place || pinAddressCue.MatchString(before) || pinAddressCue.MatchString(after)
	secret := pinSecretCue.MatchString(before) || pinSecretCue.MatchString(after)
	if place || (address && !secret) {
		return "postal"
	}
	if secret {
		return "secret"
	}
	return "unsure"
}

// withholdCards withholds every card number: a number that passes the card checksum and is written as one (in
// fours) or named as one, unless the word before names another identifier ("A/c 50100123456789").
func withholdCards(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range cardNumber.FindAllStringIndex(text, -1) {
		s, e := m[0], m[1]
		if (s > 0 && (isWordByte(text[s-1]) || text[s-1] == '-')) || (e < len(text) && (isWordByte(text[e]) || text[e] == '-')) {
			continue
		}
		raw := text[s:e]
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, raw)
		if len(digits) < 13 || len(digits) > 19 || !luhn(digits) {
			continue
		}
		from := s - 30
		if from < 0 {
			from = 0
		}
		before := text[from:s]
		lastCard, lastOther := lastEnd(cardWord, before), lastEnd(notCardWord, before)
		if lastOther > lastCard || (lastCard < 0 && !cardGrouped.MatchString(raw)) {
			continue
		}
		b.WriteString(text[last:s])
		b.WriteString(Marker)
		last = e
	}
	b.WriteString(text[last:])
	return b.String()
}

// lastEnd is where the last match of re in s ends, or -1.
func lastEnd(re *regexp.Regexp, s string) int {
	ms := re.FindAllStringIndex(s, -1)
	if len(ms) == 0 {
		return -1
	}
	return ms[len(ms)-1][1]
}

// luhn reports whether digits pass the card checksum.
func luhn(digits string) bool {
	sum := 0
	for i := 0; i < len(digits); i++ {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// tokenish reports whether a value reads as a machine's token: long, with letters and digits.
func tokenish(v string) bool {
	if len(v) < 16 {
		return false
	}
	letter, digit := false, false
	for _, r := range v {
		letter = letter || unicode.IsLetter(r)
		digit = digit || unicode.IsDigit(r)
	}
	return letter && digit
}

// allDigits reports whether s is digits only.
func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// isWordByte reports whether c is an ASCII letter, digit or underscore.
func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
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

// asksForSecret is a question whose answer is a secret: a password, a one-time code, a PIN (not a postal "PIN code").
var asksForSecret = regexp.MustCompile(`(?i)\b(?:password|passcode|passphrase|otp|one[- ]time|verification\s+code|security\s+code|auth(?:entication)?\s+code|2fa|mfa|totp|cvv|cvc|api[ _-]?key|access\s+token|secret|[mt]-?pin|pin)\b`)

// postalAsked is a question about a postal code, whose answer is no secret.
var postalAsked = regexp.MustCompile(`(?i)\bpin[\s_-]*code\b|\bpincode\b|\bpostal\b|\bzip\b`)

// Reply is a person's answer to a question the worker asked, as the records keep it: Text, and when the question
// asked for a secret (a password, an OTP, a PIN), every word of the answer with a digit or a mark in it withheld too,
// so a bare "552901" answering "What is the OTP?" is not kept (review 2026-11-03: only a labelled value was).
func Reply(question, reply string) string {
	reply = Text(reply)
	if reply == "" || !asksForSecret.MatchString(question) || (postalAsked.MatchString(question) && !secretAsked(question)) {
		return reply
	}
	fields := strings.Fields(reply)
	for _, f := range fields {
		bare := strings.TrimRight(f, ".,;:!?)")
		if bare == "" || strings.HasPrefix(bare, Marker) || len(bare) < 3 || !hasDigitOrMark(bare) {
			continue
		}
		reply = strings.Replace(reply, bare, Marker, 1)
	}
	return reply
}

// secretAsked reports whether a question asks for a secret other than a postal code.
func secretAsked(question string) bool {
	return asksForSecret.MatchString(postalAsked.ReplaceAllString(question, " "))
}
