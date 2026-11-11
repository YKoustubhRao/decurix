package withhold

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MarkedValues is what a message the person marked secret makes secret, the same set the agent holds for it
// (agent/decurix_agent/computer/privacy.py marked_values; shared vectors in testdata/marked-values.json):
//   - the message whole (HeldValues of it withheld as Secret: as typed and with its spaces folded, and the secrets its
//     structure spells out), and the message without the block of files it brings, its spaces folded;
//   - each of its words that has a value's shape: 4 or more code points, with a digit or a punctuation or symbol
//     character in it, the characters .,;:!?"'()[]{} at its ends aside (a hyphen is a mark: "correct-horse-battery");
//   - but not a word that is data by its structure: an amount or a number, a date, a time, an e-mail address, a URL,
//     a host name, a contraction, one figure over another (a ratio).
//
// A person marks a message because they did not label the value in it: the recorder held only the whole marked text,
// so an unlabelled value the model restated in its own commands was kept in 42 recorded calls (live L1, 2026-11-04).
//
// Where Go holds more than the agent, on the safe side: the agent also leaves out a word the files a message brings
// hold (it can read them; the control plane cannot), and reads digits of other scripts and NFKC forms as figures, which
// Go keeps as values. The recorder withholds a value of 4 or more code points wherever it stands (llmrec.ReplaceHeld),
// where the agent withholds these words as whole tokens only.
func MarkedValues(text string) []string {
	values := HeldValues(text, Secret)
	body := strings.Join(strings.Fields(withoutFiles(text)), " ")
	if body != "" {
		values = append(values, body)
		for _, word := range strings.Fields(body) {
			word = strings.Trim(word, edgePunctuation)
			if valueShaped(word) && !isData(word) {
				values = append(values, word)
			}
		}
	}
	seen := map[string]bool{}
	out := values[:0]
	for _, v := range values {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// filesMark begins the block a request ends with that names the files it brought (httpapi attachmentsMark, the
// agent's attachments.MARK).
const filesMark = "Files attached to this request, saved on this desktop:"

// edgePunctuation is a sentence's own punctuation, trimmed from a word's ends before its shape is read.
const edgePunctuation = ".,;:!?\"'()[]{}"

// withoutFiles is a message without the block of files it brought.
func withoutFiles(text string) string {
	if i := strings.Index(text, filesMark); i >= 0 {
		return text[:i]
	}
	return text
}

// valueShaped reports whether a word has a value's shape: 4 or more code points, with a digit, a punctuation or a
// symbol character in it. A hyphen is a mark: a passphrase of hyphenated words is a value (review of freeze 2, A2;
// agent privacy._value_shaped).
func valueShaped(word string) bool {
	if utf8.RuneCountInString(word) < 4 {
		return false
	}
	for _, r := range word {
		if unicode.IsDigit(r) || unicode.Is(unicode.No, r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return true
		}
	}
	return false
}

var (
	timeWord    = regexp.MustCompile(`(?i)^[0-9]{1,2}[:.][0-9]{2}(?::[0-9]{2})?(?:[ap]\.?m\.?)?$`)
	emailWord   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	urlWord     = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.-]*://|www\.)\S+`)
	hostWord    = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$`)
	contraction = regexp.MustCompile(`^\pL+['\x{2019}]\pL+$`)
	// ratioWord is one figure over another: a rate or a share ("100/118").
	ratioWord = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?/[0-9]+(?:\.[0-9]+)?$`)
)

// isData reports whether a value-shaped word is data by its structure, as the agent reads it (privacy._is_data).
func isData(word string) bool {
	return isAmount(word) || isDate(word) || timeWord.MatchString(word) || emailWord.MatchString(word) ||
		urlWord.MatchString(word) || hostWord.MatchString(word) || contraction.MatchString(word) ||
		ratioWord.MatchString(word)
}

var (
	currencyWord  = regexp.MustCompile(`(?i)^(?:rs|inr|rupees?|usd|eur|gbp)\.?`)
	amountFigure  = regexp.MustCompile(`^([0-9][0-9,.]*)(%|[₹$€£¥])?$`)
	leadingZero   = regexp.MustCompile(`^0[0-9]`)
	plainFigure   = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
	groupedFigure = regexp.MustCompile(`^[0-9]{1,3}(?:,[0-9]{3})+(?:\.[0-9]+)?$`)
	lakhFigure    = regexp.MustCompile(`^[0-9]{1,2}(?:,[0-9]{2})+,[0-9]{3}(?:\.[0-9]+)?$`)
	twoWaysFigure = regexp.MustCompile(`^[0-9.,]+$`)
)

// isAmount reports whether a word (no white space in it) writes one number, as values.parse_amount reads it: a sign,
// a currency word or sign before it, a % or a currency sign after it, the figure plain, grouped by thousands or by
// lakhs, or with separators that read two ways (still a figure, so data). A leading zero is an identifier, not a
// figure.
func isAmount(word string) bool {
	text := word
	if strings.HasSuffix(text, "/-") {
		text = strings.TrimSuffix(text, "/-")
	} else if (strings.HasSuffix(text, "-") || strings.HasSuffix(text, "−")) && utf8.RuneCountInString(text) > 1 {
		text = strings.TrimSuffix(strings.TrimSuffix(text, "-"), "−")
	}
	// A sign, then a currency word (only before a figure or a sign) or a currency sign; when either was there, a
	// second sign may follow ("Rs -500").
	rest, read := text, false
	if r, n := utf8.DecodeRuneInString(rest); r == '+' || r == '-' || r == '\u2212' {
		rest, read = rest[n:], true
	}
	if m := currencyWord.FindString(rest); m != "" {
		if r, _ := utf8.DecodeRuneInString(rest[len(m):]); r >= '0' && r <= '9' || r == '+' || r == '-' || r == '\u2212' {
			rest, read = rest[len(m):], true
		}
	} else if r, n := utf8.DecodeRuneInString(rest); strings.ContainsRune("₹$€£¥", r) {
		rest, read = rest[n:], true
	}
	if read {
		if r, n := utf8.DecodeRuneInString(rest); r == '+' || r == '-' || r == '\u2212' {
			rest = rest[n:]
		}
		text = rest
	}
	if strings.HasPrefix(text, ".") && len(text) > 1 && text[1] >= '0' && text[1] <= '9' {
		text = "0" + text
	}
	m := amountFigure.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	digits := m[1]
	if leadingZero.MatchString(digits) {
		return false
	}
	if plainFigure.MatchString(digits) || groupedFigure.MatchString(digits) || lakhFigure.MatchString(digits) {
		return true
	}
	return twoWaysFigure.MatchString(digits) && strings.ContainsAny(digits, ".,")
}

const dateTime = `(?:[T,]+([0-9]{1,2}):([0-9]{2})(?::([0-9]{2}))?(?:\.[0-9]+)?(?:[AaPp]\.?[Mm]\.?)?(?:Z|[+-][0-9]{2}:?[0-9]{2})?)?`

var (
	yearFirstDate = regexp.MustCompile(`^([0-9]{4})[/.\-]([0-9]{1,2})[/.\-]([0-9]{1,2})` + dateTime + `$`)
	digitDate     = regexp.MustCompile(`^([0-9]{1,2})[/.\-]([0-9]{1,2})[/.\-]([0-9]{4}|[0-9]{2})` + dateTime + `$`)
	dayNamedDate  = regexp.MustCompile(`(?i)^([0-9]{1,2})(?:st|nd|rd|th)?[/.\-]+([a-z]{3,9})\.?[/.\-,]+([0-9]{4}|[0-9]{2})` + dateTime + `$`)
	monthNames    = map[string]int{"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3, "april": 4,
		"apr": 4, "may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7, "august": 8, "aug": 8, "september": 9,
		"sep": 9, "sept": 9, "october": 10, "oct": 10, "november": 11, "nov": 11, "december": 12, "dec": 12}
)

// isDate reports whether a word names a day, as values.parse_date reads it with no order known: year first, a digit
// date (one whose day and month could be either way is still a date), or a day with a named month. A figure that is
// no real day (2026-13-04, 29/02/2025) is not a date.
func isDate(word string) bool {
	if m := yearFirstDate.FindStringSubmatch(word); m != nil {
		return realDay(atoi(m[1]), atoi(m[2]), atoi(m[3]))
	}
	if m := digitDate.FindStringSubmatch(word); m != nil {
		p1, p2, y := atoi(m[1]), atoi(m[2]), year(m[3])
		switch {
		case p1 > 12:
			return realDay(y, p2, p1)
		case p2 > 12 || p1 == p2:
			return realDay(y, p1, p2)
		default:
			return p1 >= 1 && p2 >= 1
		}
	}
	if m := dayNamedDate.FindStringSubmatch(word); m != nil {
		if month := monthNames[strings.ToLower(m[2])]; month != 0 {
			return realDay(year(m[3]), month, atoi(m[1]))
		}
	}
	return false
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// year reads a year as the agent does: two digits are this century's up to twenty years ahead, else the last.
func year(s string) int {
	y := atoi(s)
	if len(s) != 2 {
		return y
	}
	if 2000+y <= time.Now().Year()+20 {
		return 2000 + y
	}
	return 1900 + y
}

// realDay reports whether y-m-d is a day of the calendar.
func realDay(y, m, d int) bool {
	if y < 1 || m < 1 || m > 12 || d < 1 {
		return false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Year() == y && int(t.Month()) == m && t.Day() == d
}
