package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An Office document or a PDF named in the step is handed to the operator
// before any model is asked for text, and nothing is written. Live, a
// workbook request saved the model's "I can't provide a valid .xlsx file as
// raw text" as totals.xlsx and was reported done.
func TestFileWriteHandsDocumentsToTheOperator(t *testing.T) {
	ws := newTestWorkspace(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}
	// No model: reaching planFile would fail, so a pass proves nothing asked one.

	for _, step := range []string{
		"Create totals.xlsx in my work folder with two rows: Rent 1500 and Power 220, and a Total row",
		"write the summary into report.docx",
		"save the letter as letter.PDF",
	} {
		result, err := NewFileWrite().Run(rc, Subtask{Step: step, Type: "file-write"})
		if err != nil {
			t.Fatalf("%q: %v", step, err)
		}
		if result.Verified {
			t.Errorf("%q: reported written", step)
		}
		if got, _ := result.Meta["reroute"].(string); got != "operator" {
			t.Errorf("%q: reroute = %q, want operator", step, got)
		}
		if note, _ := result.Meta["rerouteNote"].(string); !strings.Contains(note, "office") {
			t.Errorf("%q: the operator is not told to use office: %q", step, note)
		}
	}

	entries, _ := os.ReadDir(ws.Root())
	for _, e := range entries {
		if isDocumentFormat(e.Name()) {
			t.Errorf("%s was written", filepath.Join(ws.Root(), e.Name()))
		}
	}
}

func TestIsDocumentFormat(t *testing.T) {
	for path, want := range map[string]bool{
		"a.xlsx": true, "dir/b.DOCX": true, "c.pdf": true, "d.ods": true,
		"notes.txt": false, "data.csv": false, "page.html": false, "": false, "xlsx": false,
	} {
		if got := isDocumentFormat(path); got != want {
			t.Errorf("isDocumentFormat(%q) = %v, want %v", path, got, want)
		}
	}
}

// What a model writes when it cannot know the content is a question for the
// person, not a file. Live, "I need your usual message sign-off text before I
// can write signoff.txt." was saved as signoff.txt and reported written.
func TestAQuestionForThePersonIsNotAFile(t *testing.T) {
	for content, want := range map[string]bool{
		"I need your usual message sign-off text before I can write signoff.txt.\n": true,
		"ASK: What is your usual sign-off?\n":                                       true,
		"Please provide the sign-off you would like me to use.":                     true,
		"What is your preferred sign-off?":                                          true,
		"Warm regards,\nKoustubh\n":                                                 false,
		"I need to buy milk\nand eggs\nand bread\nand tea\nand rice\n":              false,
		"# Notes\n\nPlease provide feedback by Friday.\n":                           false,
		"": false,
	} {
		if got := asksThePerson(content) != ""; got != want {
			t.Errorf("asksThePerson(%q) = %v, want %v", content, got, want)
		}
	}
	if got := asksThePerson("ASK: What is your usual sign-off?"); got != "What is your usual sign-off?" {
		t.Errorf("the question = %q", got)
	}
}

func TestADoneThatAsksThePersonIsAQuestion(t *testing.T) {
	for answer, want := range map[string]bool{
		"I need your sign-off text to write signoff.txt, and that information is not available on this computer.": true,
		"What is your usual message sign-off?":                   true,
		"Please provide the customer's name.":                    false, // no "you": said about the task, kept as an answer
		"I can't find notes.txt in /etc.":                        false,
		"I cannot reach example.com: the name does not resolve.": false,
		"The kernel version is 6.6.87.":                          false,
		"Wrote signoff.txt with your sign-off.":                  false,
	} {
		if got := doneAsksThePerson(answer) != ""; got != want {
			t.Errorf("doneAsksThePerson(%q) = %v, want %v", answer, got, want)
		}
	}
}

// A README with code examples is written whole: keeping only its first fenced
// block turned it into one line. A reply that is one fenced block is
// unwrapped; a code file with several blocks is not guessed at.
func TestFileBodyKeepsFencesThatAreContent(t *testing.T) {
	readme := "# Tool\n\nInstall:\n\n```bash\nmake install\n```\n\nRun:\n\n```bash\ntool --help\n```\n"
	got, err := fileBody("README.md", readme)
	if err != nil || got != readme {
		t.Fatalf("README = %q (%v), want it whole", got, err)
	}
	if got, err := fileBody("tree.py", "```python\nprint(1)\n```\n"); err != nil || got != "print(1)\n" {
		t.Errorf("one fenced block = %q (%v), want it unwrapped", got, err)
	}
	if got, err := fileBody("tree.py", "Here it is:\n```python\nprint(1)\n```\nEnjoy."); err != nil || got != "print(1)\n" {
		t.Errorf("one block with prose = %q (%v), want the block", got, err)
	}
	if got, err := fileBody("tree.py", "print(1)\n"); err != nil || got != "print(1)\n" {
		t.Errorf("plain content = %q (%v)", got, err)
	}
	if _, err := fileBody("tree.py", "```python\na = 1\n```\nand\n```python\nb = 2\n```\n"); err == nil {
		t.Error("two code blocks for one code file were written as if one were certain")
	}
}

// A version number is not a file name: "Go 1.27" saved a file named 1.27.
func TestPathFromRequestIgnoresVersionNumbers(t *testing.T) {
	for step, want := range map[string]string{
		"write a note about Go 1.27 to notes.md":            "notes.md",
		"write a note that we upgraded to 1.27":             "",
		"save the release notes for version 3.5 as rel.txt": "rel.txt",
		"write hello into out/greeting.txt":                 "out/greeting.txt",
	} {
		if got := pathFromRequest(step); got != want {
			t.Errorf("pathFromRequest(%q) = %q, want %q", step, got, want)
		}
	}
}

// The whole README the model wrote is what lands on disk, and is verified by
// reading it back.
func TestFileWriteWritesTheWholeReadme(t *testing.T) {
	readme := "# Tool\n\n```bash\nmake\n```\n\nMore text.\n\n```bash\ntool\n```\n"
	chat := &scriptedChat{replies: []string{"PATH: README.md\nCONTENT:\n" + readme}}
	ws := newTestWorkspace(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws, Provider: chat, Model: "m"}
	res, err := NewFileWrite().Run(rc, Subtask{Step: "write a README for the tool into README.md", Type: "file-write"})
	if err != nil || !res.Verified {
		t.Fatalf("result = %+v (%v)", res, err)
	}
	got, _ := os.ReadFile(filepath.Join(ws.Root(), "README.md"))
	if string(got) != readme {
		t.Errorf("on disk = %q, want %q", got, readme)
	}
}
