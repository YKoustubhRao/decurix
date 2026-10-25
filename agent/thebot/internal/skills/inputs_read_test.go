package skills

import (
	"strings"
	"testing"
)

// The facts the reviewer weighs: what the work saved or copied, and what the screens offered.

func TestTheDocumentsTheWorkSavedAreKnown(t *testing.T) {
	cases := map[string][]string{
		`cd '/home/headless/PC/UAT/06. SEPTEMBER' && curl -fsSLo 'Invoice_451.pdf' 'http://x/451.pdf' && curl -fsSLo 'IMG_0932.jpg' 'http://x/img'`: {"Invoice_451.pdf", "IMG_0932.jpg"},
		`cp ~/Downloads/PB_KCP-778.pdf '/home/headless/PC/UAT/06. SEPTEMBER/Purchases/'`:                                                            {"PB_KCP-778.pdf"},
		`wget -O report.html http://x`: nil,
		`grep -o 'x' a.pdf`:            nil,
	}
	for command, want := range cases {
		got := fetchedDocuments(command)
		if len(got) != len(want) {
			t.Errorf("%q: got %v, want %v", command, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: got %v, want %v", command, got, want)
			}
		}
	}
}

// Found live (2026-10-23): the taught task's notes are a fact the reviewer weighs, whole.
func TestWhatThePersonTaughtIsKeptWhole(t *testing.T) {
	st := &operatorState{Request: "file it"}
	long := "Task gstr1-prepare. " + strings.Repeat("step text ", 100) + "When asked: one B2C (Others) line per place of supply and rate."
	st.noteTaught("gui skill gstr1-prepare", shellResult{Output: long})
	st.noteTaught("gui skills gst", shellResult{Output: "a list"})          // a search, not the teaching
	st.noteTaught("gui guide tally", shellResult{Output: "x", ExitCode: 1}) // failed
	if len(st.Taught) != 1 || !strings.Contains(st.Taught[0], "one B2C (Others) line per place of supply and rate") {
		t.Fatalf("taught = %q", st.Taught)
	}
	st.noteTaught("gui skill gstr1-prepare", shellResult{Output: "read again"})
	if len(st.Taught) != 1 || !strings.Contains(st.Taught[0], "read again") {
		t.Errorf("a second reading did not replace the first: %q", st.Taught)
	}
	if facts := st.reviewFacts("done", nil, ""); !strings.Contains(facts, "WHAT THE PERSON TAUGHT") {
		t.Error("the reviewer was not shown it")
	}
}

func TestATaughtTaskRunByNameIsKnown(t *testing.T) {
	if taughtRunName("gui do gstr1-prepare --param x=1") != "gstr1-prepare" || taughtRunName("gui skills gst") != "" {
		t.Error("taught run name")
	}
	st := &operatorState{}
	st.noteTaught("gui skill gstr1-prepare", shellResult{Output: "Skill gstr1-prepare: 75 steps"})
	if !st.hasTaught("gstr1-prepare") || st.hasTaught("other") {
		t.Error("hasTaught")
	}
}

func TestTheDocumentsAScreenShowedAreKnown(t *testing.T) {
	st := &operatorState{}
	st.noteScreenDocs("gui observe", "e12 link \"Invoice_457.pdf\"\ne13 link \"Invoice_458_Karavali_Supermart.pdf\"")
	st.noteScreenDocs("office read x.xlsx", "Invoice_999.pdf in a cell") // not a screen
	if len(st.ScreenDocs) != 2 || st.ScreenDocs[1] != "Invoice_458_Karavali_Supermart.pdf" {
		t.Errorf("screen documents = %v", st.ScreenDocs)
	}
	// Found live (2026-10-25): a chat list's link previews another conversation's last message; that names a
	// document, it does not offer it. An attachment, its download link and what the browser saved do.
	st = &operatorState{}
	st.noteScreenDocs("gui observe", "e3 link \"Meera Kini (Manipal Interiors)📎 89_sales_invoice_Asha_Bhat.pdf\"\n"+
		"e7 text \"Bill_455.pdf\"\ne8 link \"Download Bill_455.pdf\"\ne9 text \"PB_778.pdf 12 kB\"\n"+
		"e10 paragraph \"Rohan said he would send 197_sales_invoice.pdf tomorrow evening\"")
	st.noteScreenDocs("gui click e8", "Clicked link \"Download Bill_456.pdf\" (e8).\nDownloaded: /home/headless/Downloads/Bill_456.pdf (3,120 bytes).")
	if want := "Bill_455.pdf PB_778.pdf Bill_456.pdf"; strings.Join(st.ScreenDocs, " ") != want {
		t.Errorf("screen documents = %v, want %s", st.ScreenDocs, want)
	}
	// A name with spaces is kept whole, from its download link or the path the browser saved it at - never "2).jpg".
	st = &operatorState{}
	st.noteScreenDocs("gui observe", "e3 text \"Scan 2026-09-28 at 10.15.32 (2).jpg\"\n"+
		"e4 link \"Download Scan 2026-09-28 at 10.15.32 (2).jpg\"\ne5 paragraph \"please find attached invoice 455.pdf\"")
	st.noteScreenDocs("gui click e4", "Clicked.\nDownloaded: /home/headless/Downloads/Photo 7 (1).jpeg (3,120 bytes); "+
		"/home/headless/Downloads/Bill 9.pdf (88 bytes).")
	if want := "Scan 2026-09-28 at 10.15.32 (2).jpg|Photo 7 (1).jpeg|Bill 9.pdf"; strings.Join(st.ScreenDocs, "|") != want {
		t.Errorf("names with spaces = %q, want %s", st.ScreenDocs, want)
	}
}
