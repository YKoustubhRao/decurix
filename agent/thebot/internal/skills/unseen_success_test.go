package skills

import (
	"strings"
	"testing"
)

// A DONE said again after every command failed, quoting nothing they printed, is from memory: it is put to the person
// as not checked, never reported as verified (T-01).
func TestDoneFromMemoryAfterEveryCommandFailedIsNotChecked(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\nno_such_program_here -r",
		"COMMAND:\ncat /no/such/file",
		"DONE:\nThe version is 5.10.124.",
		"DONE:\nThe version is 5.10.124.",
	}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "what is the version?", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified {
		t.Fatalf("a DONE nothing showed was verified: %+v", res)
	}
	if res.Meta["needsUserInput"] != true || !strings.Contains(res.Output, "Not checked") {
		t.Fatalf("want a not-checked question to the person, got %+v", res)
	}
}

// The failure itself can be the answer: a DONE that quotes what the failed command printed stands (T-01).
func TestDoneQuotingTheFailureStands(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\ncat /no/such/file",
		"DONE:\nThe file is not there.",
		"DONE:\nThe file is not there: cat said \"No such file or directory\".",
	}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "is /no/such/file there?", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.Meta["needsUserInput"] == true {
		t.Fatalf("a DONE quoting the error should stand, got %+v", res)
	}
}

func TestQuotesFailure(t *testing.T) {
	outs := []string{"cat: /no/such/file: No such file or directory"}
	for _, c := range []struct {
		answer string
		want   bool
	}{
		{"It failed: No such file or directory.", true},
		{"cat: /no/such/file: no such file  or directory", true},
		{"The file is missing.", false},
		{"cat said no", false}, // shorter than a quote
		{"", false},
	} {
		if got := quotesFailure(outs, c.answer); got != c.want {
			t.Errorf("quotesFailure(%q) = %v, want %v", c.answer, got, c.want)
		}
	}
	if quotesFailure(nil, "No such file or directory") {
		t.Error("nothing failed, so nothing can be quoted")
	}
}
