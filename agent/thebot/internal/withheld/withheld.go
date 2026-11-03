// Package withheld carries a job's secret values (PLAN-P2 C14): the values the person marked secret, or the control
// plane withheld, told to soul as data on the job ("secrets" on /api/chat), never in the prompt. The model is given
// the request as it is, so the work can be done; nothing soul writes down, logs into a job, serves or stores keeps a
// copy (Text, Holds, Whole).
//
// Values ride on the job's context (With, From). They are never persisted.
package withheld

import (
	"context"
	"sort"
	"strings"
)

// Mark is what a secret value is replaced with.
const Mark = "[secret withheld]"

// MinLen is the shortest value withheld: a shorter one would blank ordinary text everywhere it occurs.
const MinLen = 3

// MaxValues bounds the values one job may be told: a job sent more is refused (soul's /api/chat), never cut, so a
// job's own value is never dropped to make room.
const MaxValues = 1000

// MaxValueLen bounds one value a job may be told.
const MaxValueLen = 64 << 10

type key struct{}

// Values is a job's secret values, longest first (so a value inside another is never left half-replaced).
type Values []string

// Clean is values trimmed, without blanks, values under MinLen or repeats, longest first. It never drops a value
// for room: the caller bounds what it accepts (MaxValues, MaxValueLen).
func Clean(values []string) Values {
	seen := map[string]bool{}
	var out Values
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < MinLen || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// With returns ctx carrying values (cleaned) on top of any it already carries.
func With(ctx context.Context, values []string) context.Context {
	all := append(append([]string{}, From(ctx)...), values...)
	v := Clean(all)
	if len(v) == 0 {
		return ctx
	}
	return context.WithValue(ctx, key{}, v)
}

// From is the secret values ctx carries.
func From(ctx context.Context) Values {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(key{}).(Values)
	return v
}

// Text is text with every one of values replaced by Mark.
func (v Values) Text(text string) string {
	for _, s := range v {
		if strings.Contains(text, s) {
			text = strings.ReplaceAll(text, s, Mark)
		}
	}
	return text
}

// Shown is text as it may leave soul: Mark when the whole of it is secret, else Text. Withhold first, then shorten
// or quote: a value cut or escaped before it is withheld no longer matches, and its part leaks.
func (v Values) Shown(text string) string {
	if v.Whole(text) {
		return Mark
	}
	return v.Text(text)
}

// Holds reports whether text holds any of values.
func (v Values) Holds(text string) bool {
	for _, s := range v {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// Whole reports whether text, trimmed, is one of values: the whole request was secret, so none of it is kept.
func (v Values) Whole(text string) bool {
	t := strings.TrimSpace(text)
	for _, s := range v {
		if t == s {
			return true
		}
	}
	return false
}
