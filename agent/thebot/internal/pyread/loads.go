// Loads: Python's json.loads, read as CPython's own decoder (its C scanner) reads a model's answer.

package pyread

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// The bounds Loads holds where CPython has its own: MaxIntDigits is int()'s default limit on the digits of an integer
// (sys.get_int_max_str_digits(), 4300 since 3.11 and its backports: json.loads refuses a longer literal); MaxDepth is
// how deep lists and objects may nest - CPython stops near its recursion limit (about 1000, less the caller's frames),
// so this is set well under it: deeper text is refused here, never read where Python would refuse it.
const (
	MaxIntDigits = 4300
	MaxDepth     = 200
)

// errNotJSON is text json.loads refuses (Python's JSONDecodeError, a ValueError).
var errNotJSON = errors.New("not JSON as Python's json.loads reads it")

// Loads is json.loads(text) as CPython reads it: Python's white space only between tokens (space, tab, newline,
// return), NaN, Infinity and -Infinity as numbers, a string with no raw control character, a high and a low surrogate
// escape joined, a repeated key's last value at its first place, an integer of at most MaxIntDigits digits. A lone
// surrogate escape, which Python keeps as it is, is U+FFFD here (Go's strings cannot hold one); nesting deeper than
// MaxDepth is refused.
func Loads(text string) (Value, error) {
	p := &loader{s: text}
	p.space()
	v, err := p.value(0)
	if err != nil {
		return Value{}, err
	}
	p.space()
	if p.i != len(p.s) {
		return Value{}, errExtraData
	}
	return v, nil
}

// loader reads one JSON text, byte by byte (every token's first character is ASCII).
type loader struct {
	s string
	i int
}

// space skips JSON's white space.
func (p *loader) space() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

// word takes w when the text goes on with it.
func (p *loader) word(w string) bool {
	if strings.HasPrefix(p.s[p.i:], w) {
		p.i += len(w)
		return true
	}
	return false
}

// value reads the value at p.i (the scanner's scan_once).
func (p *loader) value(depth int) (Value, error) {
	if p.i >= len(p.s) {
		return Value{}, errNotJSON
	}
	switch p.s[p.i] {
	case '"':
		s, err := p.text()
		return Value{Kind: 's', Str: s}, err
	case '{':
		return p.object(depth + 1)
	case '[':
		return p.list(depth + 1)
	}
	switch {
	case p.word("null"):
		return Value{Kind: 'z'}, nil
	case p.word("true"):
		return Value{Kind: 'b', Bool: true}, nil
	case p.word("false"):
		return Value{Kind: 'b'}, nil
	case p.word("NaN"):
		return Value{Kind: 'n', Str: "NaN"}, nil
	case p.word("Infinity"):
		return Value{Kind: 'n', Str: "Infinity"}, nil
	case p.word("-Infinity"):
		return Value{Kind: 'n', Str: "-Infinity"}, nil
	}
	return p.number()
}

// digits skips ASCII digits and says whether there was one.
func (p *loader) digits() bool {
	start := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	return p.i > start
}

// number reads a number as _match_number_unicode does: a sign, 0 or digits not led by 0, a fraction only when a digit
// follows the point, an exponent only when a digit follows it (else the e is left for the caller, which refuses it).
func (p *loader) number() (Value, error) {
	start := p.i
	if p.i < len(p.s) && p.s[p.i] == '-' {
		p.i++
	}
	switch {
	case p.i < len(p.s) && p.s[p.i] == '0':
		p.i++
	case p.i < len(p.s) && p.s[p.i] >= '1' && p.s[p.i] <= '9':
		p.digits()
	default:
		return Value{}, errNotJSON
	}
	whole := true
	if p.i+1 < len(p.s) && p.s[p.i] == '.' && p.s[p.i+1] >= '0' && p.s[p.i+1] <= '9' {
		p.i++
		p.digits()
		whole = false
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		back := p.i
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if p.digits() {
			whole = false
		} else {
			p.i = back
		}
	}
	literal := p.s[start:p.i]
	if whole && len(strings.TrimPrefix(literal, "-")) > MaxIntDigits {
		return Value{}, errNotJSON // ValueError: Exceeds the limit for integer string conversion
	}
	return Value{Kind: 'n', Str: literal}, nil
}

// hex4 reads four hex digits at p.i.
func (p *loader) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	var r rune
	for _, c := range []byte(p.s[p.i : p.i+4]) {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	p.i += 4
	return r, true
}

// text reads a string as scanstring does with strict=True: no raw control character, the JSON escapes, a high and a
// low surrogate escape joined into one character.
func (p *loader) text() (string, error) {
	p.i++ // the opening quote
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", errNotJSON // unterminated
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c < 0x20:
			return "", errNotJSON // Invalid control character
		case c != '\\':
			b.WriteByte(c)
			p.i++
			continue
		}
		p.i++
		if p.i >= len(p.s) {
			return "", errNotJSON
		}
		e := p.s[p.i]
		p.i++
		switch e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", errNotJSON // Invalid \uXXXX escape
			}
			if r >= 0xd800 && r <= 0xdbff && strings.HasPrefix(p.s[p.i:], `\u`) {
				back := p.i
				p.i += 2
				low, ok := p.hex4()
				switch {
				case !ok:
					return "", errNotJSON
				case low >= 0xdc00 && low <= 0xdfff:
					r = 0x10000 + (r-0xd800)<<10 + (low - 0xdc00)
				default:
					p.i = back
				}
			}
			if r >= 0xd800 && r <= 0xdfff {
				r = utf8.RuneError
			}
			b.WriteRune(r)
		default:
			return "", errNotJSON // Invalid \escape
		}
	}
}

// object reads an object: keys are strings, no trailing comma.
func (p *loader) object(depth int) (Value, error) {
	if depth > MaxDepth {
		return Value{}, errNotJSON
	}
	p.i++ // {
	v := Value{Kind: 'o'}
	p.space()
	if p.word("}") {
		return v, nil
	}
	for {
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return Value{}, errNotJSON // Expecting property name enclosed in double quotes
		}
		key, err := p.text()
		if err != nil {
			return Value{}, err
		}
		p.space()
		if !p.word(":") {
			return Value{}, errNotJSON
		}
		p.space()
		item, err := p.value(depth)
		if err != nil {
			return Value{}, err
		}
		v.set(key, item)
		p.space()
		if p.word("}") {
			return v, nil
		}
		if !p.word(",") {
			return Value{}, errNotJSON
		}
		p.space()
	}
}

// list reads a list: no trailing comma.
func (p *loader) list(depth int) (Value, error) {
	if depth > MaxDepth {
		return Value{}, errNotJSON
	}
	p.i++ // [
	v := Value{Kind: 'l'}
	p.space()
	if p.word("]") {
		return v, nil
	}
	for {
		item, err := p.value(depth)
		if err != nil {
			return Value{}, err
		}
		v.Items = append(v.Items, item)
		p.space()
		if p.word("]") {
			return v, nil
		}
		if !p.word(",") {
			return Value{}, errNotJSON
		}
		p.space()
	}
}
