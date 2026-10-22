package skills

import "testing"

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

func TestTheDocumentsAScreenShowedAreKnown(t *testing.T) {
	st := &operatorState{}
	st.noteScreenDocs("gui observe", "e12 link \"Invoice_457.pdf\"\ne13 link \"Invoice_458_Karavali_Supermart.pdf\"")
	st.noteScreenDocs("office read x.xlsx", "Invoice_999.pdf in a cell") // not a screen
	if len(st.ScreenDocs) != 2 || st.ScreenDocs[1] != "Invoice_458_Karavali_Supermart.pdf" {
		t.Errorf("screen documents = %v", st.ScreenDocs)
	}
}
