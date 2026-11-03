package withheld

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValues(t *testing.T) {
	v := Clean([]string{" ab ", "", "abc", "abcdef", "abc"})
	if len(v) != 3 || v[0] != "ab" || v[1] != "abc" || v[2] != "abcdef" {
		t.Fatalf("Clean = %v (blank and repeated values go; the order given stays)", v)
	}
	if got := v.Text("x abcdef y abc z ab"); got != "x "+Mark+" y "+Mark+" z "+Mark {
		t.Fatalf("Text = %q", got)
	}
	if !v.Holds("..abc..") || v.Holds("xabcx") || !v.Whole("  abcdef ") || v.Whole("abcdef!") {
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
		many[i] = strings.Repeat("x", i+1)
	}
	if got := Clean(many); len(got) != 300 {
		t.Fatalf("Clean dropped values for room: %d kept", len(got))
	}
	if got := v.Shown(" abcdef "); got != Mark {
		t.Fatalf("Shown of a wholly secret text = %q", got)
	}
	if got := v.Shown("is abc"); got != "is "+Mark {
		t.Fatalf("Shown = %q", got)
	}
}

// WrittenForms gives exactly the agent's approval.written_forms, in its order (testdata made from it).
func TestWrittenFormsAreTheAgents(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "written-forms.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Value string   `json:"value"`
			Forms []string `json:"forms"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 5 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		if got := WrittenForms(c.Value); !reflect.DeepEqual(got, c.Forms) {
			t.Errorf("WrittenForms(%q) =\n %q\nwant\n %q", c.Value, got, c.Forms)
		}
	}
}

// Each written form of a value is withheld where a command or a URL carries it.
func TestEveryWrittenFormIsWithheld(t *testing.T) {
	v := Clean([]string{`it's "q" $x`})
	cases := []struct{ line, want string }{
		{`gui type e3 'it'\''s "q" $x'`, `gui type e3 '` + Mark + `'`},                                   // the '\'' form inside single quotes
		{`gui type e3 'it'"'"'s "q" $x'`, `gui type e3 ` + Mark},                                         // shlex.quote
		{`printf "it's \"q\" \$x"`, `printf "` + Mark + `"`},                                             // inside double quotes
		{`{"text":"it's \"q\" $x"}`, `{"text":"` + Mark + `"}`},                                          // JSON
		{`curl https://example.test/?p=it%27s%20%22q%22%20%24x`, `curl https://example.test/?p=` + Mark}, // percent
		{`curl -d p=it%27s+%22q%22+%24x https://example.test/`, `curl -d p=` + Mark + ` https://example.test/`},
	}
	for _, c := range cases {
		if got := v.Text(c.line); got != c.want {
			t.Errorf("%s\n  -> %s\n want %s", c.line, got, c.want)
		}
	}
}
