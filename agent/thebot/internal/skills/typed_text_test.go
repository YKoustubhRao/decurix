package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"thebot/internal/llm"
	"thebot/internal/trace"
)

func TestMaskTypedText(t *testing.T) {
	cases := map[string]string{
		`gui type e3 'plain-value-77'`:                     `gui type e3 [text]`,
		`gui type e3 "two words"`:                          `gui type e3 [text]`,
		`gui type e3 two bare words`:                       `gui type e3 [text]`,
		`gui type D1 '=SUM(A1:A3)'`:                        `gui type D1 [text]`,
		`gui type textbox "Site title" 'Decurix'`:          `gui type textbox "Site title" [text]`,
		`gui type 'textbox "Total"' '5900.00'`:             `gui type 'textbox "Total"' [text]`,
		`gui type --role textbox --name Total 5900.00`:     `gui type --role textbox --name Total [text]`,
		`gui type --append e4 'more'`:                      `gui type --append e4 [text]`,
		`gui type 'only the text'`:                         `gui type [text]`,
		`gui observe && gui type e3 'x1' && gui key Enter`: `gui observe && gui type e3 [text] && gui key Enter`,
		"skill:operator -> $ gui type e3 'v9'":             "skill:operator -> $ gui type e3 [text]",
		"gui type e3 - <<'EOF'\nline one\nline two\nEOF":   "gui type e3 - <<'EOF'\n[text]\nEOF",
		`gui click e5`:                                     `gui click e5`,
		`Ran gui type e3 "v1" and it is in the field`:      `Ran gui type e3 [text] and it is in the field`,
		"Done: `gui type e3 'v2'` worked.":                 "Done: `gui type e3 [text]` worked.",
		`echo gui type is a command`:                       `echo gui type is [text]`,
	}
	for in, want := range cases {
		if got := MaskTypedText(in); got != want {
			t.Errorf("MaskTypedText(%q)\n  = %q\nwant %q", in, got, want)
		}
	}
	if got := TypedTexts(`gui type e3 "it's \"x\"" && gui type e4 'two words'`); strings.Join(got, "|") != `it's "x"|two words` {
		t.Errorf("TypedTexts = %q", got)
	}
}

// F-S3 (defence in depth): a gui type of a value nobody marked as a secret - not sent on the job, not given after a
// credential's name, not into a field listed as a password - never appears in the step line, the job log or the
// trace. The command that runs still types it.
func TestAnUnflaggedTypedValueIsNeverShown(t *testing.T) {
	bin := t.TempDir()
	// A gui that prints what it was asked to type, as an app's echo would.
	if err := os.WriteFile(filepath.Join(bin, "gui"), []byte("#!/bin/sh\necho typed \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	const value = "quince-orchard-77"
	chat := &scriptedChat{replies: []string{"COMMAND:\ngui type e3 '" + value + "'", "DONE:\ntyped it"}}
	dir := t.TempDir()
	rc := operatorContext(t, llm.Metered(trace.Calls(chat)))
	var logged []string
	rc.OnLog = func(l string) { logged = append(logged, l) }
	rc.Ctx = trace.WithWriter(llm.WithTags(context.Background(), llm.Tags{Task: "t_ty", Job: "jy"}), trace.New(dir))
	res, err := NewOperator().Run(rc, Subtask{Step: "type the code " + value + " into the field e3", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(logged, "\n")
	raw, _ := os.ReadFile(filepath.Join(dir, "t_ty.jsonl"))
	if strings.Contains(all, value) || strings.Contains(string(raw), value) {
		t.Fatalf("the typed value was shown:\n%s\n%s", all, raw)
	}
	if !strings.Contains(all, "$ gui type e3 [text]") {
		t.Fatalf("no masked step line:\n%s", all)
	}
	// The command that ran typed the value: the model, which reads the command's real output, was shown it.
	// (On Windows the stand-in gui is not what the operator's shell resolves; the Linux run checks this.)
	if runtime.GOOS != "windows" && (len(chat.calls) < 2 || !strings.Contains(chat.calls[1][len(chat.calls[1])-1].Content, value)) {
		t.Fatalf("the command that ran did not get the value: %+v", chat.calls)
	}
	_ = res
}
