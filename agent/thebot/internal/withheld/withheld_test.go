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
	many := make([]string, 100)
	for i := range many {
		many[i] = strings.Repeat("x", i+MinLen)
	}
	if len(Clean(many)) != MaxValues {
		t.Fatal("not bounded")
	}
}
