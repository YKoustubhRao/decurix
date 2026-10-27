package skills

import "testing"

// Found live (2026-10-26): links whose whole text was a bill's file name with spaces were kept as their last word
// ("Prabhu.docx"), which no read matched, and the review reported two entered bills as never read.
func TestADocumentNameWithSpacesIsKeptWhole(t *testing.T) {
	st := &operatorState{}
	st.noteScreenDocs("gui observe", "e12 link \"1191 Naveen Prabhu.docx\"\ne13 link \"1192 Mala Resorts.docx\"\n"+
		"e14 link \"Download 1193 Shobha Kulal.docx\"\ne15 text \"Attachments (3)\"\ne16 link \"Open the folder October bills - Karkala Steel\"\n"+
		"e17 text \"1188 Coastal Infra Projects.docx\"\ne18 text \"please find attached invoice 455.pdf\"")
	// (A sentence that ends in a file name mentions it and offers nothing: not kept, as before.)
	want := map[string]bool{"1191 Naveen Prabhu.docx": true, "1192 Mala Resorts.docx": true, "1193 Shobha Kulal.docx": true,
		"1188 Coastal Infra Projects.docx": true}
	for _, d := range st.ScreenDocs {
		if !want[d] {
			t.Errorf("kept %q", d)
		}
		delete(want, d)
	}
	for d := range want {
		t.Errorf("did not keep %q; kept %v", d, st.ScreenDocs)
	}
}
