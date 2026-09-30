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
