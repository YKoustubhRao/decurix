// Package pyread is Python's own reading of text and JSON, for the decisions soul makes in Python's place (plan rule
// 3, phase 2: internal/judge, internal/escalation): a string's length and cuts in code points, strip() and split() on
// Python's white space, json.loads with its key order and its duplicate keys (the last value, at the first key's place),
// str() and repr() of what it decodes, float() and int() of a value. Each decision's parity corpus, recorded from the
// Python code, holds the two sides to the same reading.
//
// Where Python and Go differ outside what a model answers with, Go is the stricter (and the decision the one that does
// not act): see Decode, Loads and FloatOf for each case.
package pyread

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

// Space is Python's str.isspace: Go's white space and the four separators \x1c to \x1f.
func Space(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Strip is str.strip().
func Strip(s string) string { return strings.TrimFunc(s, Space) }

// Split is str.split(): the words between runs of white space.
func Split(s string) []string { return strings.FieldsFunc(s, Space) }

// Len is len() of a Python str.
func Len(s string) int { return utf8.RuneCountInString(s) }

// Head is s[:n] of a Python str.
func Head(s string, n int) string {
	i := 0
	for at := range s {
		if i == n {
			return s[:at]
		}
		i++
	}
	return s
}

// Tail is s[-n:] of a Python str.
func Tail(s string, n int) string {
	if count := Len(s); count > n {
		return s[len(Head(s, count-n)):]
	}
	return s
}

// Value is a JSON value as Python's json.loads makes it: an object keeps its keys' order, a number its literal ("NaN",
// "Infinity" and "-Infinity" too, which Loads reads as Python does).
type Value struct {
	Kind  byte // 's' string, 'n' number, 'b' bool, 'z' null, 'l' list, 'o' object
	Str   string
	Bool  bool
	Keys  []string
	Items []Value
}

// Get is dict.get: the value under key and whether it is there.
func (v Value) Get(key string) (Value, bool) {
	for i, k := range v.Keys {
		if k == key {
			return v.Items[i], true
		}
	}
	return Value{}, false
}

// errExtraData is JSON followed by more than white space (Python: "Extra data").
var errExtraData = errors.New("extra data after the JSON value")

// Decode decodes one JSON value, nothing after it, with Go's decoder (internal/judge's reading): NaN and Infinity as
// literals do not decode here, where Python's json.loads reads them, and a lone surrogate escape is U+FFFD.
func Decode(text string) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := readGo(dec)
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errExtraData
		}
		return Value{}, err
	}
	return v, nil
}

// readGo reads the next JSON value off dec, objects in their keys' order.
func readGo(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return Value{}, err
	}
	switch t := tok.(type) {
	case json.Delim:
		v := Value{Kind: 'l'}
		if t == '{' {
			v.Kind = 'o'
		}
		for dec.More() {
			key := ""
			if v.Kind == 'o' {
				k, err := dec.Token()
				if err != nil {
					return Value{}, err
				}
				key, _ = k.(string)
			}
			item, err := readGo(dec)
			if err != nil {
				return Value{}, err
			}
			if v.Kind == 'o' {
				v.set(key, item)
				continue
			}
			v.Items = append(v.Items, item)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return Value{}, err
		}
		return v, nil
	case string:
		return Value{Kind: 's', Str: t}, nil
	case json.Number:
		return Value{Kind: 'n', Str: string(t)}, nil
	case bool:
		return Value{Kind: 'b', Bool: t}, nil
	case nil:
		return Value{Kind: 'z'}, nil
	}
	return Value{}, errors.New("an unexpected JSON token")
}

// set is d[key] = item of a Python dict: a repeated key keeps its first place and takes the last value.
func (v *Value) set(key string, item Value) {
	for i, k := range v.Keys {
		if k == key {
			v.Items[i] = item
			return
		}
	}
	v.Keys = append(v.Keys, key)
	v.Items = append(v.Items, item)
}

// isInt is whether a number's literal is an integer (json.loads makes an int of it, else a float).
func isInt(literal string) bool {
	digits := strings.TrimPrefix(literal, "-")
	return digits != "" && strings.Trim(digits, "0123456789") == ""
}

// Truthy is bool() of the value.
func (v Value) Truthy() bool {
	switch v.Kind {
	case 's':
		return v.Str != ""
	case 'b':
		return v.Bool
	case 'n':
		return parseFloatAnyRange(v.Str) != 0 // 1e-400 is 0.0 in Python too; NaN is true in both
	case 'l', 'o':
		return len(v.Items) > 0
	}
	return false
}

// StrOr is str(x or ""): "" for a value that is not there or is falsy.
func StrOr(v Value, present bool) string {
	if !present || !v.Truthy() {
		return ""
	}
	return Str(v)
}

// Str is str() of a decoded JSON value.
func Str(v Value) string {
	switch v.Kind {
	case 's':
		return v.Str
	case 'z':
		return "None"
	case 'b':
		if v.Bool {
			return "True"
		}
		return "False"
	case 'n':
		if isInt(v.Str) {
			digits := strings.TrimLeft(strings.TrimPrefix(v.Str, "-"), "0")
			switch {
			case digits == "":
				return "0" // int("-0")
			case strings.HasPrefix(v.Str, "-"):
				return "-" + digits
			}
			return digits
		}
		return FloatRepr(parseFloatAnyRange(v.Str))
	case 'l':
		parts := make([]string, len(v.Items))
		for i, item := range v.Items {
			parts[i] = Repr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case 'o':
		parts := make([]string, len(v.Items))
		for i, item := range v.Items {
			parts[i] = ReprString(v.Keys[i]) + ": " + Repr(item)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

// Repr is repr() of a decoded JSON value.
func Repr(v Value) string {
	if v.Kind == 's' {
		return ReprString(v.Str)
	}
	return Str(v)
}

// parseFloatAnyRange is float() of a JSON number's literal: past the range it is an infinity or zero, as in Python
// ("NaN", "Infinity" and "-Infinity" are read as Python reads them).
func parseFloatAnyRange(literal string) float64 {
	f, _ := strconv.ParseFloat(literal, 64)
	return f
}

// FloatRepr is repr() of a Python float: the shortest digits that read back, fixed between 1e-4 and 1e16 (with ".0"
// for a whole number), else with an exponent of at least two digits ("1e+16", "1e-05").
func FloatRepr(f float64) string {
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

// floatText is the grammar of float() of a string (ASCII digits; "_" between digits), after its white space is gone.
var floatText = func() *regexp.Regexp {
	digits := `[0-9](?:_?[0-9])*`
	number := `(?:(?:` + digits + `)?\.` + digits + `|` + digits + `\.?)`
	return regexp.MustCompile(`^[+-]?(?:` + number + `(?:[eE][+-]?` + digits + `)?|(?i:inf|infinity|nan))$`)
}()

// intText is the grammar of int() of a string in base 10, after its white space is gone.
var intText = regexp.MustCompile(`^[+-]?[0-9](?:_?[0-9])*$`)

// FloatOf is float(x) for a decoded JSON value as internal/judge reads it, and false where Python raises TypeError or
// ValueError (None, a list, a dict, a string that is not a number). A string is stripped of str.isspace and read in
// ASCII digits only: float() of other digits, which Python reads, is no number here. Float is Python's float() exactly.
func FloatOf(v Value) (float64, bool) {
	if v.Kind == 's' {
		return floatOfText(Strip(v.Str))
	}
	return Float(v)
}

// Float is float(x) for a decoded JSON value exactly as Python reads it, and false where Python raises: TypeError
// (None, a list, a dict), ValueError (a string that is not a number) or OverflowError (an int past a float's range;
// Python's float() raises it, and every reader here takes it as no number). A string's digits and white space beyond
// ASCII are first made ASCII (ToASCII), then the string is stripped of ASCII white space.
func Float(v Value) (float64, bool) {
	switch v.Kind {
	case 'n':
		f := parseFloatAnyRange(v.Str)
		if isInt(v.Str) && math.IsInf(f, 0) {
			return 0, false // float() of an int past the range: OverflowError
		}
		return f, true
	case 'b':
		if v.Bool {
			return 1, true
		}
		return 0, true
	case 's':
		s, ok := ToASCII(v.Str)
		if !ok {
			return 0, false
		}
		return floatOfText(strings.Trim(s, asciiSpace))
	}
	return 0, false
}

// floatOfText is float() of a string already stripped: its grammar, then its value.
func floatOfText(s string) (float64, bool) {
	if !floatText.MatchString(s) {
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

// asciiSpace is the white space int() and float() strip once the text is ASCII (Py_ISSPACE): not \x1c to \x1f, which
// str.strip() takes and float() refuses.
const asciiSpace = " \t\n\v\f\r"

// ToASCII is CPython's _PyUnicode_TransformDecimalAndSpaceToASCII, which int() and float() apply to a string first:
// ASCII stays, other white space is a space, any other decimal digit (Unicode Nd) is its ASCII digit, and anything
// else makes the text no number (false).
func ToASCII(s string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 127:
			b.WriteRune(r)
		case Space(r):
			b.WriteByte(' ')
		case unicode.IsDigit(r):
			b.WriteByte(byte('0' + digitValue(r)))
		default:
			return "", false
		}
	}
	return b.String(), true
}

// digitValue is a Unicode decimal digit's value: digits are encoded in runs of ten from zero, so it is the distance
// from the start of its run, modulo ten.
func digitValue(r rune) int {
	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}
	return int(r-start) % 10
}

// Int is int(x) for a decoded JSON value as Python reads it, and false where Python raises: TypeError (None, a list, a
// dict), ValueError (a string that is not a whole number, a NaN) or OverflowError (int() of an infinity). A value past
// int64's range is clamped to it (every reader here takes it as out of range either way).
func Int(v Value) (int64, bool) {
	switch v.Kind {
	case 'n':
		if isInt(v.Str) {
			return intOfDigits(v.Str), true
		}
		f := parseFloatAnyRange(v.Str)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		f = math.Trunc(f)
		switch {
		case f >= math.MaxInt64:
			return math.MaxInt64, true
		case f <= math.MinInt64:
			return math.MinInt64, true
		}
		return int64(f), true
	case 'b':
		if v.Bool {
			return 1, true
		}
		return 0, true
	case 's':
		s, ok := ToASCII(v.Str)
		if !ok {
			return 0, false
		}
		s = strings.Trim(s, asciiSpace)
		if !intText.MatchString(s) {
			return 0, false
		}
		return intOfDigits(strings.ReplaceAll(strings.TrimPrefix(s, "+"), "_", "")), true
	}
	return 0, false
}

// intOfDigits is the value of an optionally signed run of ASCII digits, clamped to int64's range.
func intOfDigits(s string) int64 {
	negative := strings.HasPrefix(s, "-")
	digits := strings.TrimLeft(strings.TrimPrefix(s, "-"), "0")
	if len(digits) > 18 {
		if negative {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	n, _ := strconv.ParseInt(digits, 10, 64) // "" is 0
	if negative {
		return -n
	}
	return n
}

// ReprString is repr() of a Python str: single quotes unless the text holds a ' and no ", the quote and backslash
// escaped, \t \n \r by name, other control characters and anything Python does not print as \xhh, \uhhhh or
// \Uhhhhhhhh.
func ReprString(s string) string {
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
			b.WriteString(`\x` + hexOf(r, 2))
		case r < 0x7f || printable(r):
			b.WriteRune(r)
		case r <= 0xff:
			b.WriteString(`\x` + hexOf(r, 2))
		case r <= 0xffff:
			b.WriteString(`\u` + hexOf(r, 4))
		default:
			b.WriteString(`\U` + hexOf(r, 8))
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// hexOf is r in lower-case hex, padded with zeros to width.
func hexOf(r rune, width int) string {
	h := strconv.FormatInt(int64(r), 16)
	return strings.Repeat("0", width-len(h)) + h
}

// printable is str.isprintable() of one character past ASCII: not a control, format, private-use, surrogate or
// unassigned character, nor a separator other than the space.
func printable(r rune) bool {
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp, unicode.Zs) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}
