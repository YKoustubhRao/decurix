package withheld

import (
	"context"
	"strings"
	"testing"
)

func TestValues(t *testing.T) {
	v := Clean([]string{" ab ", "", "abc", "abcdef", "abc"})
	if len(v) != 2 || v[0] != "abcdef" || v[1] != "abc" {
		t.Fatalf("Clean = %v (short, blank and repeated values go; longest first)", v)
	}
	if got := v.Text("x abcdef y abc z"); got != "x "+Mark+" y "+Mark+" z" {
		t.Fatalf("Text = %q", got)
	}
	if !v.Holds("..abc..") || v.Holds("ab") || !v.Whole("  abcdef ") || v.Whole("abcdef!") {
		t.Fatal("Holds/Whole")
	}
	ctx := With(With(context.Background(), []string{"first-value"}), []string{"second-value"})
	if got := From(ctx); len(got) != 2 {
		t.Fatalf("From = %v", got)
	}
	if From(context.Background()) != nil || With(context.Background(), nil) != context.Background() {
		t.Fatal("an empty list changes the context")
	}
	many := make([]string, 300)
	for i := range many {
		many[i] = strings.Repeat("x", i+MinLen)
	}
	if got := Clean(many); len(got) != 300 || got[len(got)-1] != "xxx" {
		t.Fatalf("Clean dropped values for room: %d kept", len(got))
	}
	if got := v.Shown(" abcdef "); got != Mark {
		t.Fatalf("Shown of a wholly secret text = %q", got)
	}
	if got := v.Shown("is abc"); got != "is "+Mark {
		t.Fatalf("Shown = %q", got)
	}
}
