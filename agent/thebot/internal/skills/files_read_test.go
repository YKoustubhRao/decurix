package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Found live (2026-10-21): Word invoices read by a python script were not known as read, and their figures were
// refused as invented at the portal's Save.
func TestTheFilesACommandNamesAreNotedAsRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs Linux absolute paths: a command's paths are read as /-rooted (namedPathRe), and the test's temp dir has a drive letter")
	}
	root := t.TempDir()
	folder := filepath.Join(root, "GST Returns", "6. SEPTEMBER")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"786 Kamath Jewellers.docx", "SalesRegister_Sep2026.xlsx", "notes.bin"} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	taskFile := filepath.Join(root, "task")
	if err := os.WriteFile(taskFile, []byte("t7abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECURIX_TASK_FILE", taskFile)
	t.Setenv("DECURIX_DIR", filepath.Join(root, "state"))

	noteFilesRead("python3 - <<'PY'\nbase = Path('" + folder + "')\nfor f in sorted(base.glob('*.docx')): print(f)\nPY")
	noteFilesRead(`office read "` + filepath.Join(folder, "SalesRegister_Sep2026.xlsx") + `" | head`)
	noteFilesRead(`echo hello`)

	data, err := os.ReadFile(filepath.Join(root, "state", "computer", "evidence", "t7abc.files"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"786 Kamath Jewellers.docx", "SalesRegister_Sep2026.xlsx"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s not noted: %q", want, got)
		}
	}
	if strings.Contains(got, "notes.bin") {
		t.Errorf("a file of no readable kind was noted: %q", got)
	}
}

// Found live (2026-10-21): "Lift$2026" was typed as "Lift026" once the nudges to use stdin ran out.
func TestATypedTextWithADollarIsTypedAsWritten(t *testing.T) {
	cases := map[string]string{
		`gui type e2334 "Lift$2026"`:                                `gui type e2334 'Lift$2026'`,
		`gui type e1 "x" && gui type e2 "Pa$$w0rd" && gui click e3`: `gui type e1 "x" && gui type e2 'Pa$$w0rd' && gui click e3`,
		`gui type e2 "it's $5 \"cash\""`:                            `gui type e2 'it'\''s $5 "cash"'`,
	}
	for in, want := range cases {
		if got := literalTyping(in); got != want {
			t.Errorf("literalTyping(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	for _, same := range []string{`gui type e2 "plain text"`, `gui type e2 'Lift$2026'`, `gui type e2 "US$ 5"`, `echo "$HOME"`} {
		if got := literalTyping(same); got != same {
			t.Errorf("literalTyping(%q) changed it to %q", same, got)
		}
	}
}

// Found live (2026-10-21): values printed on Word invoices a script had read were refused as never read.
func TestValuesInAFileTheWorkNamedAreTraced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh (its office stand-in) and Linux absolute paths: a command's paths are read as /-rooted (namedPathRe), and the test's temp dir has a drive letter")
	}
	root := t.TempDir()
	invoice := filepath.Join(root, "786 Kamath Jewellers.docx")
	if err := os.WriteFile(invoice, []byte("Annual maintenance | 9987 | 1 nos | 26,500.00"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A stand-in for the office command: prints the file.
	if err := os.WriteFile(filepath.Join(bin, "office"), []byte("#!/bin/sh\ncat \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	taskFile := filepath.Join(root, "task")
	if err := os.WriteFile(taskFile, []byte("t7xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECURIX_TASK_FILE", taskFile)
	t.Setenv("DECURIX_DIR", filepath.Join(root, "state"))
	noteFilesRead("python3 read_invoices.py '" + invoice + "'")

	st := groundingState("Prepare the GSTR-1 from the invoices in the September folder.")
	command := `gui type e40 "9987" && gui type e41 "26500.00"`
	if n := groundingNudge(st, command); n == "" {
		t.Fatal("the values were traced before anything was read")
	}
	o := &OperatorSkill{}
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}}
	if !o.readNamedSources(rc, st) {
		t.Fatal("nothing was read")
	}
	if n := groundingNudge(st, command); n != "" {
		t.Fatalf("values printed on the invoice the work named are still refused: %s", n)
	}
	if o.readNamedSources(rc, st) {
		t.Fatal("a file was read twice")
	}
}

// Found live (2026-10-21): asked where "900" came from, the person's reply did not settle it and the same question
// came back every 16 seconds.
func TestThePersonsAnswerSettlesTheValuesTheyWereAskedAbout(t *testing.T) {
	o := &OperatorSkill{}
	st := groundingState("Prepare the GSTR-1 from the invoices.")
	st.AskedUntraced, st.UntracedNudges = []string{"900", "9346.27"}, 2
	// A plain yes settles them with no reading; any other reply on a local brain is not known, and not known is not
	// vouched (review T-11) - the question tells the person to reply yes (plainYesNote).
	o.resume(RunContext{Ctx: context.Background(), OnLog: func(string) {}, HumanReply: "use your judgement"}, Subtask{}, st)
	if groundingNudge(st, `gui type e5 "900"`) != "" || st.UntracedNudges != 0 || st.AskedUntraced != nil {
		t.Fatalf("a plain yes left the values untraced (nudges %d)", st.UntracedNudges)
	}
	st2 := groundingState("Prepare the GSTR-1 from the invoices.")
	st2.AskedUntraced = []string{"900"}
	o.resume(RunContext{Ctx: context.Background(), OnLog: func(string) {}, HumanReply: "No - add up the quantities again."}, Subtask{}, st2)
	if groundingNudge(st2, `gui type e5 "900"`) == "" {
		t.Fatal("a no made the value traced")
	}
}

// Found live (2026-10-21): an application's own option ("KGS-KILOGRAMS") for a source's "KGS" was refused.
func TestAnOptionLabelIsTracedByItsCodeOrDescription(t *testing.T) {
	st := groundingState("Prepare the GSTR-1 from the invoices.")
	st.noteSeen("Sona masoori rice | 1006 | 500 KGS | 52.00\nPlace of Supply: Karnataka\nwire rope 90 mtr")
	for _, ok := range []string{`gui select e9 "KGS-KILOGRAMS"`, `gui select e9 "MTR-METERS"`, `gui select e4 "29-Karnataka"`} {
		if n := groundingNudge(st, ok); n != "" {
			t.Errorf("%s was refused: %s", ok, n)
		}
	}
	for _, bad := range []string{`gui select e9 "BOX-BOXES"`, `gui select e4 "32-Kerala"`, `gui type e2 "Green Furnishings"`} {
		if n := groundingNudge(st, bad); n == "" {
			t.Errorf("%s was let through", bad)
		}
	}
}

// Found live (2026-10-21): totals a script printed were not the task's recorded working.
func TestWhatACommandPrintedIsKept(t *testing.T) {
	root := t.TempDir()
	taskFile := filepath.Join(root, "task")
	if err := os.WriteFile(taskFile, []byte("t7out"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECURIX_TASK_FILE", taskFile)
	t.Setenv("DECURIX_DIR", filepath.Join(root, "state"))
	noteCommandOutput(`python3 -c 'print(4350+25423.72+25423.72+12711.86)'`, "67909.3\n")
	noteCommandOutput(`gui observe`, "Screen 1280x720 ...")
	data, err := os.ReadFile(filepath.Join(root, "state", "computer", "evidence", "t7out.outputs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "67909.3") || strings.Contains(string(data), "Screen 1280x720") {
		t.Fatalf("kept %q", data)
	}
}
