package skills

import (
	"strings"
	"testing"
)

// Found live (2026-10-26): six bills read in one loop that named them outright; the output's masking of the command's
// own values turned "1191 Naveen Prabhu.docx:" into "· Naveen Prabhu.docx:", and two bills were "never read".
func TestFilesALoopNamesAreReadEvenWhenTheOutputMasksTheirNumbers(t *testing.T) {
	st := &operatorState{ScreenDocs: []string{"1191 Naveen Prabhu.docx", "1193 Mrs. Shobha Kulal.docx", "1188 Coastal Infra Projects.docx"}}
	cmd := `for f in /home/h/Downloads/'1188 Coastal Infra Projects.docx' /home/h/Downloads/'1191 Naveen Prabhu.docx' /home/h/Downloads/'1193 Mrs. Shobha Kulal.docx'; do echo "FILE:$f"; office read "$f" | sed -n '1,120p'; echo '---'; done`
	out := "FILE:/home/h/Downloads/· Coastal Infra Projects.docx\n· Coastal Infra Projects.docx: Word document\nBill No. : ·\nTotal 76,700.00\n---\n" +
		"FILE:/home/h/Downloads/· Naveen Prabhu.docx\n· Naveen Prabhu.docx: Word document\nTotal 8,674.00\n---\n" +
		"FILE:/home/h/Downloads/· Mrs. Shobha Kulal.docx\n· Mrs. Shobha Kulal.docx: Word document\nTotal 19,470.00\n---\n"
	st.noteEvidence(cmd, out)
	var docs []string
	for _, u := range st.Units {
		if u.Kind == "document" {
			docs = append(docs, u.Source)
			switch u.Source {
			case "1191 Naveen Prabhu.docx":
				if !strings.Contains(u.Text, "8,674.00") || strings.Contains(u.Text, "19,470.00") {
					t.Errorf("1191 holds its own part: %q", u.Text)
				}
			case "1193 Mrs. Shobha Kulal.docx":
				if !strings.Contains(u.Text, "19,470.00") {
					t.Errorf("1193 holds its own part: %q", u.Text)
				}
			}
		}
	}
	if strings.Join(docs, "|") != "1188 Coastal Infra Projects.docx|1191 Naveen Prabhu.docx|1193 Mrs. Shobha Kulal.docx" {
		t.Fatalf("documents = %v", docs)
	}
	for _, gap := range st.coverageGaps() {
		if strings.Contains(gap, "never read") {
			t.Errorf("a file the loop named is still a gap: %s", gap)
		}
	}
	// A file the loop names whose header the output lost altogether still counts as read, holding the whole output.
	st = &operatorState{ScreenDocs: []string{"1192 Mala Resorts.docx"}}
	st.noteEvidence(`for f in '/w/1192 Mala Resorts.docx'; do office read "$f"; done`, "Word document\nTotal 26,904.00")
	docs = nil
	for _, u := range st.Units {
		if u.Kind == "document" {
			docs = append(docs, u.Source+" = "+u.Text)
		}
	}
	if len(docs) != 1 || !strings.HasPrefix(docs[0], "1192 Mala Resorts.docx = ") || !strings.Contains(docs[0], "26,904.00") {
		t.Fatalf("documents = %v", docs)
	}
}
