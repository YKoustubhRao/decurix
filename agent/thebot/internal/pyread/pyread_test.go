package pyread

import (
	"math"
	"strings"
	"testing"
)

func TestPythonsFloatRepr(t *testing.T) {
	for f, want := range map[float64]string{1: "1.0", 100: "100.0", 1e16: "1e+16", 1e15: "1000000000000000.0",
		0.0001: "0.0001", 0.00001: "1e-05", 1.5e-7: "1.5e-07", 1e100: "1e+100", -2.5: "-2.5", 0.30000000000000004: "0.30000000000000004",
		123456789.125: "123456789.125"} {
		if got := FloatRepr(f); got != want {
			t.Errorf("repr(%v) = %q, want %q", f, got, want)
		}
	}
}

// TestLoadsReadsWhatPythonReads: json.loads's own answers (recorded from CPython 3.11) - NaN and the infinities, a
// joined surrogate pair, the last of a repeated key at its first place - and what it refuses.
func TestLoadsReadsWhatPythonReads(t *testing.T) {
	// u is a JSON \u escape of hex (written so, as no source file here holds one whole).
	u := func(hex string) string { return `\` + "u" + hex }
	for text, want := range map[string]string{
		`{"a": NaN, "b": -Infinity, "c": Infinity}`:                          "{'a': nan, 'b': -inf, 'c': inf}",
		`{"a": 1e400, "b": -0.0, "c": -0, "d": 1E2}`:                         "{'a': inf, 'b': -0.0, 'c': 0, 'd': 100.0}",
		`{"a": "` + u("d83d") + u("DE00") + `", "b": "` + u("00e9") + `\t"}`: "{'a': '\U0001f600', 'b': 'é\\t'}",
		`{"k": 1, "j": 2, "k": 3}`:                                           "{'k': 3, 'j': 2}",
		" {\r\n\t\"a\" : [ ] , \"b\":{}}\n":                                  "{'a': [], 'b': {}}",
		`{"a": "` + u("d800") + `x", "b": "` + u("d800") + u("0041") + `"}`:  "{'a': '\U0000fffdx', 'b': '\U0000fffdA'}", // Python keeps a lone surrogate
	} {
		v, err := Loads(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if got := Str(v); got != want {
			t.Errorf("%s: str() is %s, want %s", text, got, want)
		}
	}
	for _, text := range []string{`{"a": 1,}`, `{"a": [1,]}`, `{'a': 1}`, `{"a": 01}`, `{"a": 1.}`, `{"a": .5}`,
		`{"a": 1e}`, `{"a": "x` + "\n" + `"}`, `{"a": "\x"}`, `{"a": "\u12"}`, `{"a": nan}`, `{"a": +1}`, `{"a" 1}`,
		`{"a": 1} x`, `{"a": -}`, `{"a": tru}`, "{\"a\": 1 }", `{"a": ` + strings.Repeat("9", MaxIntDigits+1) + `}`,
		`{"a": ` + strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth) + `}`} {
		if _, err := Loads(text); err == nil {
			t.Errorf("%.80s: read, where Python refuses it", text)
		}
	}
	if _, err := Loads(`{"a": ` + strings.Repeat("9", MaxIntDigits) + `}`); err != nil {
		t.Errorf("an integer of %d digits: %v", MaxIntDigits, err)
	}
}

// TestIntAndFloatArePythons: int() and float() of what a model answers, as CPython 3.11 reads it.
func TestIntAndFloatArePythons(t *testing.T) {
	str := func(s string) Value { return Value{Kind: 's', Str: s} }
	num := func(s string) Value { return Value{Kind: 'n', Str: s} }
	ints := []struct {
		v    Value
		want int64
		ok   bool
	}{
		{str(" 2 "), 2, true}, {str("+2"), 2, true}, {str("1_0"), 10, true}, {str("٢"), 2, true},
		{str("\U0001D7CF"), 1, true}, {str(" 2"), 2, true}, {str("\u00852"), 2, true}, {str("\v2\f"), 2, true},
		{str("00"), 0, true}, {str("-0"), 0, true}, {str("2.0"), 0, false}, {str("\x1c2"), 0, false},
		{str("0x2"), 0, false}, {str("1__0"), 0, false}, {str("_1"), 0, false}, {str("1_"), 0, false},
		{str("+"), 0, false}, {str(""), 0, false}, {str("٢²"), 0, false}, {str("one"), 0, false},
		{num("3"), 3, true}, {num("2.9"), 2, true}, {num("-0.5"), 0, true}, {num("1e2"), 100, true},
		{num("NaN"), 0, false}, {num("Infinity"), 0, false}, {num("1e400"), 0, false},
		{num("123456789012345678901234567890"), math.MaxInt64, true}, {num("-1e300"), math.MinInt64, true},
		{Value{Kind: 'b', Bool: true}, 1, true}, {Value{Kind: 'z'}, 0, false}, {Value{Kind: 'l'}, 0, false},
	}
	for _, c := range ints {
		if got, ok := Int(c.v); got != c.want || ok != c.ok {
			t.Errorf("int(%s) = %d, %v; want %d, %v", Repr(c.v), got, ok, c.want, c.ok)
		}
	}
	floats := []struct {
		v    Value
		want float64
		ok   bool
	}{
		{str("٠.٩"), 0.9, true}, {str(" 0.9　"), 0.9, true}, {str("\v0.9"), 0.9, true},
		{str("\x1c0.9"), 0, false}, {str("0.9\x1c"), 0, false}, {str(" 0.9_5 "), 0.95, true}, {str("1e1_0"), 1e10, true},
		{str("-Infinity"), math.Inf(-1), true}, {str("high"), 0, false}, {num("1e400"), math.Inf(1), true},
		{num("1" + strings.Repeat("0", 400)), 0, false}, {Value{Kind: 'b'}, 0, true}, {Value{Kind: 'z'}, 0, false},
	}
	for _, c := range floats {
		if got, ok := Float(c.v); got != c.want || ok != c.ok {
			t.Errorf("float(%s) = %v, %v; want %v, %v", Repr(c.v), got, ok, c.want, c.ok)
		}
	}
	if f, ok := Float(str("nan")); !ok || !math.IsNaN(f) {
		t.Errorf("float('nan') = %v, %v", f, ok)
	}
}
