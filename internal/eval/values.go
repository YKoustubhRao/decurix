package eval

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// These are the grader's own value rules. They stay independent of the agent's comparator on purpose: a bug in
// one must not be hidden by the same bug in the other.

// appValue is a stored field as text: strings as they are, numbers and booleans in their JSON form.
func appValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

var digitDate = regexp.MustCompile(`^(\d{1,4})[/.-](\d{1,2})[/.-](\d{1,4})$`)

// appDate reads an app's date value as ISO: an ISO date as it is, or a digit date in the written order ("dmy" or
// "mdy"). Without a written order only ISO is a date: a digit date's order is never guessed.
func appDate(s, written string) (string, bool) {
	s = strings.TrimSpace(s)
	if isoDate.MatchString(s) {
		if _, err := time.Parse("2006-01-02", s); err == nil {
			return s, true
		}
		return "", false
	}
	m := digitDate.FindStringSubmatch(s)
	if m == nil || (written != "dmy" && written != "mdy") || len(m[3]) != 4 {
		return "", false
	}
	day, month := m[1], m[2]
	if written == "mdy" {
		day, month = m[2], m[1]
	}
	d, _ := strconv.Atoi(day)
	mo, _ := strconv.Atoi(month)
	y, _ := strconv.Atoi(m[3])
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if mo < 1 || mo > 12 || t.Day() != d || int(t.Month()) != mo {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// valueMatches compares one expected value with an app's stored value.
func valueMatches(want Value, got any, written string) bool {
	g := appValue(got)
	switch want.Kind {
	case ValueID:
		return strings.EqualFold(strings.TrimSpace(want.S), strings.TrimSpace(g))
	case ValueNumber:
		a, ok1 := parseNumber(want.S)
		b, ok2 := parseNumber(g)
		return ok1 && ok2 && a.Cmp(b) == 0
	case ValueDate:
		iso, ok := appDate(g, written)
		return ok && iso == want.S
	case ValueText:
		return strings.EqualFold(collapse(want.S), collapse(g))
	}
	return strings.TrimSpace(want.S) == strings.TrimSpace(g)
}

// recordMatches reports whether a stored record holds every expected field.
func recordMatches(want map[string]Value, got map[string]any, written string) bool {
	for field, v := range want {
		value, ok := got[field]
		if !ok || !valueMatches(v, value, written) {
			return false
		}
	}
	return true
}

func describeRecord(r map[string]Value) string {
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + r[k].String()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// numberToken is a run of digits with the separators a written number may hold (a plain space is not one: it
// would join two numbers).
var numberToken = regexp.MustCompile(`\d[\d,_'\x{00a0}\x{202f}]*(?:\.\d+)?`)

// textHoldsNumber reports whether text writes the number, digit for digit with separators aside. A number inside
// a longer run of digits does not count; a minus sign right before a token makes it negative.
func textHoldsNumber(text, number string) bool {
	want, ok := parseNumber(number)
	if !ok {
		return false
	}
	for _, loc := range numberToken.FindAllStringIndex(text, -1) {
		tok := strings.TrimRight(text[loc[0]:loc[1]], ",_'  ")
		got, ok := parseNumber(tok)
		if !ok {
			continue
		}
		if before := text[:loc[0]]; strings.HasSuffix(before, "-") || strings.HasSuffix(before, "−") {
			got.Neg(got)
		}
		if got.Cmp(want) == 0 {
			return true
		}
	}
	return false
}

// textHolds checks words in text, whitespace collapsed and case folded: all of words, and one of any.
func textHolds(text string, words, any []string) (missing []string, anyFound bool) {
	t := strings.ToLower(collapse(text))
	for _, w := range words {
		if w = strings.ToLower(collapse(w)); w != "" && !strings.Contains(t, w) {
			missing = append(missing, w)
		}
	}
	if len(any) == 0 {
		return missing, true
	}
	for _, w := range any {
		if w = strings.ToLower(collapse(w)); w != "" && strings.Contains(t, w) {
			return missing, true
		}
	}
	return missing, false
}

// documentText is a saved file's text: a Word or OpenDocument file's paragraphs, or the file itself when it is UTF-8
// text. Anything else has no text the grader can read.
func documentText(name string, b []byte) (string, error) {
	lower := strings.ToLower(name)
	part := ""
	switch {
	case strings.HasSuffix(lower, ".docx"):
		part = "word/document.xml"
	case strings.HasSuffix(lower, ".odt"):
		part = "content.xml"
	}
	if part == "" {
		if !utf8.Valid(b) {
			return "", fmt.Errorf("%s is not text, a Word document or an OpenDocument text", name)
		}
		return string(b), nil
	}
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", fmt.Errorf("%s is not a readable document", name)
	}
	for _, f := range z.File {
		if f.Name != part {
			continue
		}
		if f.UncompressedSize64 > 64<<20 {
			return "", fmt.Errorf("%s is too large to read", name)
		}
		r, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("%s is not a readable document", name)
		}
		defer r.Close()
		return xmlText(io.LimitReader(r, 64<<20))
	}
	return "", fmt.Errorf("%s has no document body", name)
}

// xmlText is a document body's character data, one line per paragraph, a tab for a tab element.
func xmlText(r io.Reader) (string, error) {
	d := xml.NewDecoder(r)
	var b strings.Builder
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("the document's body is not well-formed XML")
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.StartElement:
			if t.Name.Local == "tab" || t.Name.Local == "s" {
				b.WriteByte(' ')
			}
			if t.Name.Local == "br" || t.Name.Local == "line-break" {
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p", "h":
				b.WriteByte('\n')
			case "tc", "table-cell":
				b.WriteByte('\t')
			}
		}
	}
	return b.String(), nil
}
