package judge

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Python's own reading of text and JSON, as far as the judge's prompt and parse use it: a string's length and cuts in
// code points, strip() and split() on Python's whitespace, json.loads with its key order and its duplicate keys
// (the last value, at the first key's place), str() and repr() of what it decodes, float() of a value.
//
// Where Python and Go differ outside what a model answers with, Go is the stricter (and the judgement unsure, never
// done): NaN and Infinity as JSON literals and a lone surrogate escape do not decode here; float() of digits other
// than ASCII, which Python reads, is no number here.

// pySpace is Python's str.isspace: Go's white space and the four separators \x1c to \x1f.
func pySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pySpace) }

// pySplit is str.split(): the words between runs of white space.
func pySplit(s string) []string { return strings.FieldsFunc(s, pySpace) }

// runeCount is len() of a Python str.
func runeCount(s string) int { return utf8.RuneCountInString(s) }

// head is s[:n] of a Python str.
func head(s string, n int) string {
	i := 0
	for at := range s {
		if i == n {
			return s[:at]
		}
		i++
	}
	return s
}

// tail is s[-n:] of a Python str.
func tail(s string, n int) string {
	if count := runeCount(s); count > n {
		return s[len(head(s, count-n)):]
	}
	return s
}

// pyValue is a JSON value as Python's json.loads makes it: an object keeps its keys' order, a number its literal.
type pyValue struct {
	kind  byte // 's' string, 'n' number, 'b' bool, 'z' null, 'l' list, 'o' object
	str   string
	b     bool
	keys  []string
	items []pyValue
}

// get is dict.get: the value under key and whether it is there.
func (v pyValue) get(key string) (pyValue, bool) {
	for i, k := range v.keys {
		if k == key {
			return v.items[i], true
		}
	}
	return pyValue{}, false
}

// errExtraData is JSON followed by more than white space (Python: "Extra data").
var errExtraData = errors.New("extra data after the JSON value")

// decodePy decodes one JSON value, nothing after it.
func decodePy(text string) (pyValue, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := readPy(dec)
	if err != nil {
		return pyValue{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errExtraData
		}
		return pyValue{}, err
	}
	return v, nil
}

// readPy reads the next JSON value off dec, objects in their keys' order.
func readPy(dec *json.Decoder) (pyValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return pyValue{}, err
	}
	switch t := tok.(type) {
	case json.Delim:
		v := pyValue{kind: 'l'}
		if t == '{' {
			v.kind = 'o'
		}
		for dec.More() {
			key := ""
			if v.kind == 'o' {
				k, err := dec.Token()
				if err != nil {
					return pyValue{}, err
				}
				key, _ = k.(string)
			}
			item, err := readPy(dec)
			if err != nil {
				return pyValue{}, err
			}
			if v.kind == 'o' {
				if i := indexOf(v.keys, key); i >= 0 {
					v.items[i] = item // a repeated key: the last value, at the first one's place
					continue
				}
				v.keys = append(v.keys, key)
			}
			v.items = append(v.items, item)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return pyValue{}, err
		}
		return v, nil
	case string:
		return pyValue{kind: 's', str: t}, nil
	case json.Number:
		return pyValue{kind: 'n', str: string(t)}, nil
	case bool:
		return pyValue{kind: 'b', b: t}, nil
	case nil:
		return pyValue{kind: 'z'}, nil
	}
	return pyValue{}, errors.New("an unexpected JSON token")
}

// indexOf is the place of key among keys, or -1.
func indexOf(keys []string, key string) int {
	for i, k := range keys {
		if k == key {
			return i
		}
	}
	return -1
}

// isInt is whether a JSON number's literal is an integer (json.loads makes an int of it, else a float).
func isInt(literal string) bool { return !strings.ContainsAny(literal, ".eE") }

// truthy is bool() of the value.
func (v pyValue) truthy() bool {
	switch v.kind {
	case 's':
		return v.str != ""
	case 'b':
		return v.b
	case 'n':
		return parseFloatAnyRange(v.str) != 0 // 1e-400 is 0.0 in Python too
	case 'l', 'o':
		return len(v.items) > 0
	}
	return false
}

// pyStrOr is str(x or ""): "" for a value that is not there or is falsy.
func pyStrOr(v pyValue, present bool) string {
	if !present || !v.truthy() {
		return ""
	}
	return pyStr(v)
}

// pyStr is str() of a decoded JSON value.
func pyStr(v pyValue) string {
	switch v.kind {
	case 's':
		return v.str
	case 'z':
		return "None"
	case 'b':
		if v.b {
			return "True"
		}
		return "False"
	case 'n':
		if isInt(v.str) {
			if strings.Trim(v.str, "-0") == "" {
				return "0" // int("-0")
			}
			return v.str
		}
		return pyFloatRepr(parseFloatAnyRange(v.str))
	case 'l':
		parts := make([]string, len(v.items))
		for i, item := range v.items {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case 'o':
		parts := make([]string, len(v.items))
		for i, item := range v.items {
			parts[i] = pyReprString(v.keys[i]) + ": " + pyRepr(item)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

// pyRepr is repr() of a decoded JSON value.
func pyRepr(v pyValue) string {
	if v.kind == 's' {
		return pyReprString(v.str)
	}
	return pyStr(v)
}

// parseFloatAnyRange is float() of a JSON number's literal: past the range it is an infinity or zero, as in Python.
func parseFloatAnyRange(literal string) float64 {
	f, _ := strconv.ParseFloat(literal, 64)
	return f
}

// pyFloatRepr is repr() of a Python float: the shortest digits that read back, fixed between 1e-4 and 1e16 (with
// ".0" for a whole number), else with an exponent of at least two digits ("1e+16", "1e-05").
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // "-1.2345e+20"
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	mantissa, exponent, _ := strings.Cut(s, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exp, _ := strconv.Atoi(exponent)
	decpt := exp + 1
	if decpt > -4 && decpt <= 16 {
		switch {
		case decpt <= 0:
			return sign + "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			return sign + digits[:decpt] + "." + digits[decpt:]
		}
	}
	out := sign + digits[:1]
	if len(digits) > 1 {
		out += "." + digits[1:]
	}
	e := decpt - 1
	esign := "+"
	if e < 0 {
		esign, e = "-", -e
	}
	es := strconv.Itoa(e)
	if len(es) < 2 {
		es = "0" + es
	}
	return out + "e" + esign + es
}

// pyFloatText is the grammar of float() of a string (ASCII digits; "_" between digits), after strip().
var pyFloatText = func() *regexp.Regexp {
	digits := `[0-9](?:_?[0-9])*`
	number := `(?:(?:` + digits + `)?\.` + digits + `|` + digits + `\.?)`
	return regexp.MustCompile(`^[+-]?(?:` + number + `(?:[eE][+-]?` + digits + `)?|(?i:inf|infinity|nan))$`)
}()

// pyFloatOf is float(x) for a decoded JSON value, and false where Python raises TypeError or ValueError (None, a
// list, a dict, a string that is not a number).
func pyFloatOf(v pyValue) (float64, bool) {
	switch v.kind {
	case 'n':
		return parseFloatAnyRange(v.str), true
	case 'b':
		if v.b {
			return 1, true
		}
		return 0, true
	case 's':
		s := pyStrip(v.str)
		if !pyFloatText.MatchString(s) {
			return 0, false
		}
		bare := strings.ToLower(strings.TrimLeft(s, "+-"))
		negative := strings.HasPrefix(s, "-")
		switch bare {
		case "nan":
			return math.NaN(), true
		case "inf", "infinity":
			if negative {
				return math.Inf(-1), true
			}
			return math.Inf(1), true
		}
		return parseFloatAnyRange(strings.ReplaceAll(s, "_", "")), true
	}
	return 0, false
}

// pyReprString is repr() of a Python str: single quotes unless the text holds a ' and no ", the quote and backslash
// escaped, \t \n \r by name, other control characters and anything Python does not print as \xhh, \uhhhh or
// \Uhhhhhhhh.
func pyReprString(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range s {
		switch {
		case r == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < ' ' || r == 0x7f:
			b.WriteString(`\x` + hex2(r, 2))
		case r < 0x7f || pyPrintable(r):
			b.WriteRune(r)
		case r <= 0xff:
			b.WriteString(`\x` + hex2(r, 2))
		case r <= 0xffff:
			b.WriteString(`\u` + hex2(r, 4))
		default:
			b.WriteString(`\U` + hex2(r, 8))
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// hex2 is r in lower-case hex, padded with zeros to width.
func hex2(r rune, width int) string {
	h := strconv.FormatInt(int64(r), 16)
	return strings.Repeat("0", width-len(h)) + h
}

// pyPrintable is str.isprintable() of one character past ASCII: not a control, format, private-use, surrogate or
// unassigned character, nor a separator other than the space.
func pyPrintable(r rune) bool {
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp, unicode.Zs) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}
