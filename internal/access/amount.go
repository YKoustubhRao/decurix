package access

import (
	"regexp"
	"strconv"
	"strings"
)

// The amount reader answers one question for an approval policy: could this action move more than the policy's
// limit? Asking a second approver too often is safe; skipping one is not, so wherever a figure can be read two
// ways it takes the larger reading, and every standalone figure in the text counts once any figure in it is
// marked as money.

// figureRe finds a figure: digits with optional group or decimal separators (comma, dot, apostrophe, and the
// no-break and thin spaces that formatted numbers use).
var figureRe = regexp.MustCompile(`\d+(?:[.,'\x{00A0}\x{202F}\x{2009}]\d+)*`)

// currencyWords are ISO codes and words that name a currency, matched in any case.
const currencyWords = `rs|inr|rupees?|usd|eur|euros?|gbp|aed|sar|qar|kwd|omr|bhd|sgd|hkd|jpy|cny|rmb|aud|nzd|chf|sek|nok|dkk|zar|myr|thb|idr|lkr|npr|bdt|pkr|krw|brl|mxn|dollars?`

// currencyCodesUpper are ISO codes that are also ordinary words or names ("try", "php", "rub", "cad"); they name a
// currency only in capitals. "Pounds" is left out: it is as often a weight.
const currencyCodesUpper = `TRY|PHP|RUB|CAD`

// currencyRe is a currency word or code with no letter or digit glued to its front.
const currencyRe = `(?:(?i:` + currencyWords + `)|` + currencyCodesUpper + `)`

// moneyBeforeRe is what marks the figure after it as money: a currency sign, or a currency word or code, or
// "amount"/"total"/"sum", spaced from the figure or glued to it ("Rs450000", "INR 450000", "Rs.78", "Total:980").
var moneyBeforeRe = regexp.MustCompile(`(?:\p{Sc}|(?:^|[^\p{L}\p{N}_])(?:` + currencyRe + `|(?i:amount|total|sum))\.?:?)\s*$`)

// moneyAfterRe is what marks the figure before it as money, from where the figure ends: a currency sign, a currency
// word or code spaced or glued ("450000INR", "500 rupees"), or the Indian "/-".
var moneyAfterRe = regexp.MustCompile(`^\s*(?:\p{Sc}|` + currencyRe + `(?:[^\p{L}\p{N}_]|$)|/-)`)

// percentAfterRe: a figure followed by this is a rate, never an amount.
var percentAfterRe = regexp.MustCompile(`(?i)^\s?(?:%|per\s?cent\b|pct\b)`)

// scaleRe is a scale after a figure, spaced or glued ("4.5 lakh", "3Cr", "5L", "$50k", "1.5M", "2 bn"). A single
// letter is read as a scale too (5 L may be litres): the larger reading is the safe one. A letter followed by a
// slash is a word of its own ("M/s Acme" names a firm), except the Indian "/-" after an amount.
var scaleRe = regexp.MustCompile(`^\s*((?i:lakhs?|lacs?|crores?|cr|thousand|millions?|mn|mio|billions?|bn|l|k|m))(?:/-|[^\p{L}\p{N}_/]|$)`)

// identifierBeforeRe ends in a label that makes the unmarked figure after it an identifier, not an amount
// ("a/c 1234567890", "Ref: 778899", "Phone 9876543210", "No. 45", "PO 4500012345", "Order 1234567890"). Not
// "invoice" or "bill": "Pay bill 45000" may well be the amount.
var identifierBeforeRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(?:a/c|acc(?:oun)?t|acct|ifsc|phone|mobile|tel|utr|ref(?:erence)?|id|pin(?:\s?code)?|po|order|cheque|chq|txn|no|number|#)\.?:?\s*(?:(?:no|number|id)\.?:?\s*)?$`)

// monthRe is a month name or its usual short form.
const monthRe = `(?i:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`

// dayBeforeMonthRe follows a day of the month ("30 Nov", "1st of March").
var dayBeforeMonthRe = regexp.MustCompile(`^(?i:st|nd|rd|th)?\s*(?i:of\s+)?` + monthRe + `\b`)

// monthBeforeRe precedes a day or a year ("Nov 30", "Nov 30, 2026", "30 Nov 2026", "March 2026").
var monthBeforeRe = regexp.MustCompile(`\b` + monthRe + `\.?,?\s+(?:\d{1,2}(?i:st|nd|rd|th)?,?\s+)?$`)

// scale returns the multiplier a scale word names.
func scale(word string) float64 {
	switch strings.TrimRight(strings.ToLower(word), "s") {
	case "lakh", "lac", "l":
		return 1e5
	case "crore", "cr":
		return 1e7
	case "k", "thousand":
		return 1e3
	case "million", "mn", "mio", "m":
		return 1e6
	case "billion", "bn":
		return 1e9
	}
	return 1
}

// moneyScale reports whether a scale word is used only for money (lakh, crore), so it marks the figure as money.
func moneyScale(word string) bool {
	switch strings.TrimRight(strings.ToLower(word), "s") {
	case "lakh", "lac", "crore", "cr":
		return true
	}
	return false
}

// LargestAmount returns the largest figure in text that could be the money an action moves, or nil when there is
// none. It answers an approval threshold, so it errs high. With no figure marked as money (a currency sign, code or
// word before or after it, spaced or glued; "amount"/"total"; digit grouping; lakh/crore) the amount is unknown:
// nil, and an amount policy applies ("Pay 3 invoices" is not a payment of 3). With one, every standalone figure
// counts, marked or not, so a policy is skipped only when all of them are under its limit (review 2026-11-02 and
// 2026-11-03: "Pay 450000 ... bank charges Rs 50" and "Pay Rs450000 ... fee Rs 50" both read as 50 and skipped
// the two-approver rule). A scale (lakh, crore, Cr, L, k, M, million, bn) multiplies its figure; a figure read two
// ways (1.250 as 1250 or 1.25) takes the larger reading. A figure followed by "%" is a rate, a figure joined to
// letters, a dash, a slash or a colon (INV-2024, 27/10, 5kg, 10:30) is part of a code, a unit, a date or a time,
// an unmarked day or year beside a month name (30 Nov 2026) is a date, and an unmarked figure right after an
// identifier label (a/c, Ref, Phone, No.) is that identifier.
func LargestAmount(text string) *float64 {
	var best *float64
	sawMoney := false
	// The largest figure set aside as an identifier ("No. 4500012345"): when it is larger than every money figure,
	// which one is the money is not something words can settle, so the amount is unknown and the policy applies
	// (A-05) - never the smaller figure, which would let a large action pass a lower bar.
	var identifier float64
	for _, loc := range figureRe.FindAllStringIndex(text, -1) {
		raw, before, after := text[loc[0]:loc[1]], text[:loc[0]], text[loc[1]:]
		v, grouped, ok := readFigure(raw)
		if !ok {
			continue
		}
		markedBefore := moneyBeforeRe.MatchString(before)
		markedAfter := moneyAfterRe.MatchString(after)
		sc := scaleRe.FindStringSubmatchIndex(after)
		if !markedBefore && joined(before, true) {
			continue
		}
		if !markedAfter && sc == nil && joined(after, false) {
			continue
		}
		if percentAfterRe.MatchString(after) {
			continue
		}
		marked := markedBefore || markedAfter || grouped
		if sc != nil {
			word := after[sc[2]:sc[3]]
			v *= scale(word)
			if moneyScale(word) || moneyAfterRe.MatchString(after[sc[3]:]) {
				marked = true
			}
		}
		if !marked && isDatePart(raw, v, before, after) {
			continue
		}
		if !marked && identifierBeforeRe.MatchString(before) {
			identifier = max(identifier, v)
			continue
		}
		if marked {
			sawMoney = true
		}
		if best == nil || v > *best {
			val := v
			best = &val
		}
	}
	if !sawMoney || best != nil && identifier > *best {
		return nil
	}
	return best
}

// readFigure reads a figure that may carry group and decimal separators, and reports whether its grouping marks it
// as money. Western (1,250,000), Indian (12,50,000), European (1.250.000,00), apostrophe (1'250'000) and spaced
// (1 250 000) grouping are read; a figure that fits none (12.10.2026, 1,2,3) is not an amount. When a figure fits
// two readings (1.250 or 1,250: a decimal or a thousand) the larger is returned.
func readFigure(raw string) (value float64, grouped bool, ok bool) {
	var groups []string
	var seps []rune
	start := 0
	for i, r := range raw {
		if r < '0' || r > '9' {
			groups = append(groups, raw[start:i])
			seps = append(seps, r)
			start = i + len(string(r))
		}
	}
	groups = append(groups, raw[start:])
	if len(seps) == 0 {
		v, err := strconv.ParseFloat(raw, 64)
		return v, false, err == nil
	}
	best := -1.0
	if allSame(seps) && validGrouping(groups, seps[0]) {
		if v, err := strconv.ParseFloat(strings.Join(groups, ""), 64); err == nil {
			best = v
			// A single dot before three digits may be a decimal; only an unambiguous grouping marks money.
			grouped = seps[0] != '.' || len(seps) > 1
		}
	}
	n := len(groups)
	last := seps[len(seps)-1]
	if last == '.' || last == ',' {
		intSeps := seps[:len(seps)-1]
		if len(intSeps) == 0 || (allSame(intSeps) && intSeps[0] != last && validGrouping(groups[:n-1], intSeps[0])) {
			if v, err := strconv.ParseFloat(strings.Join(groups[:n-1], "")+"."+groups[n-1], 64); err == nil {
				if v > best {
					best = v
				}
				if len(intSeps) > 0 {
					grouped = true
				}
			}
		}
	}
	if best < 0 {
		return 0, false, false
	}
	return best, grouped, true
}

// allSame reports whether every separator is the same character.
func allSame(seps []rune) bool {
	for _, s := range seps {
		if s != seps[0] {
			return false
		}
	}
	return true
}

// validGrouping reports whether digit groups split by sep are thousands grouping: Western (a lead of 1-3 digits,
// then threes) for any separator, or Indian (a lead of 1-2, then twos, then a final three) for a comma.
func validGrouping(groups []string, sep rune) bool {
	if len(groups) < 2 || groups[0] == "" || groups[0][0] == '0' {
		return false
	}
	western := len(groups[0]) <= 3
	for _, g := range groups[1:] {
		if len(g) != 3 {
			western = false
		}
	}
	if western {
		return true
	}
	if sep != ',' || len(groups) < 3 || len(groups[0]) > 2 || len(groups[len(groups)-1]) != 3 {
		return false
	}
	for _, g := range groups[1 : len(groups)-1] {
		if len(g) != 2 {
			return false
		}
	}
	return true
}

// isDatePart reports whether an unmarked figure is a day or a year beside a month name ("30 Nov", "Nov 30",
// "Nov 2026", "30 Nov 2026").
func isDatePart(raw string, v float64, before, after string) bool {
	if len(raw) <= 2 && v >= 1 && v <= 31 {
		return dayBeforeMonthRe.MatchString(strings.TrimLeft(after, " ,")) || monthBeforeRe.MatchString(before)
	}
	if len(raw) == 4 && v >= 1900 && v <= 2199 {
		return monthBeforeRe.MatchString(before)
	}
	return false
}

// joined reports whether a figure touches a code, a unit, a date or a time on that side: a letter or digit right
// beside it, a dash or slash with a letter or digit beyond (INV-2024, 27/10), or a colon between digits (10:30).
// A full stop after the figure ends the sentence, and "/-" after it is the Indian mark for a round amount.
func joined(side string, before bool) bool {
	r := []rune(side)
	if len(r) == 0 {
		return false
	}
	at, next := len(r)-1, len(r)-2
	if !before {
		at, next = 0, 1
	}
	c := r[at]
	if isAlnum(c) {
		return true
	}
	if next < 0 || next >= len(r) {
		return false
	}
	switch c {
	case '-', '/':
		if !before && c == '/' && r[next] == '-' {
			return false
		}
		return isAlnum(r[next])
	case '.':
		return before && isAlnum(r[next]) // "v1.2" or "No.45"; after the figure a full stop ends the sentence
	case ':':
		return r[next] >= '0' && r[next] <= '9'
	}
	return false
}

// isAlnum reports whether r is a letter or a digit.
func isAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
