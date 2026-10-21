package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Round 5 P2's portal stage (2026-10-20): the guard demanded "/purchase_requisition.xlsx" (a broken path), the work
// folder's purchase_requisition.xlsx (a deliverable still to make) and the inbox copy of price_list_Oct2026.xlsx (the
// worker had read the work folder's identical copy) - and refused "10/18/2026", a date the chats gave.
func p2StageState(t *testing.T) (*operatorState, string, string) {
	t.Helper()
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "up-99758f49013a")
	work := filepath.Join(root, "work", "P2_orders_19Oct")
	for _, dir := range []string{inbox, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"price_list_Oct2026.xlsx": "PK prices", "WhatsApp Chat with Arjun Rao (Maple Leaf Bakery).txt": "18/10/2026, 09:12 - Arjun: 60 mugs please"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	request := "Stage 4 of 5 of a longer request a person made: Enter the orders in the portal.\n\n" +
		"The whole request:\nMorning! Today's orders please. Attached: 4 WhatsApp chat exports, our price list. The same files " +
		"are in the P2_orders_19Oct folder in your work folder.\nPlease:\n" +
		"1. Enter every approved order in the portal. Order date = the day they ordered.\n" +
		"4. purchase_requisition.xlsx: what we need to buy for the backorders, per SKU. Save it as " + work +
		"/purchase_requisition.xlsx.\nSave everything in P2_orders_19Oct/purchase_requisition.xlsx and so on.\n\n" +
		attachedFilesMark + "\n  " + filepath.Join(inbox, "price_list_Oct2026.xlsx") + "\n"
	st := groundingState(request, "office read "+filepath.Join(work, "price_list_Oct2026.xlsx"))
	return st, inbox, work
}

func TestTheUnreadSourcesAreOnlyRealSourcesNotYetRead(t *testing.T) {
	saved := sourceMissing
	sourceMissing = func(p string) bool { _, err := os.Stat(p); return err != nil }
	defer func() { sourceMissing = saved }()

	st, inbox, work := p2StageState(t)
	if got := unreadSources(st); len(got) != 0 {
		t.Errorf("unread = %v; want none: the deliverable is made, not read; the price list was read in the work folder", got)
	}
	for _, doc := range requestDocuments(st.Request) {
		if doc == "/purchase_requisition.xlsx" {
			t.Errorf("a relative path's tail was taken for a path: %v", requestDocuments(st.Request))
		}
	}

	// Read through its folder (a listing or a script over it, the file's name never written): the same content
	// there is the attached file read.
	st.Commands = []string{"python3 read_all.py " + work}
	if got := unreadSources(st); len(got) != 0 {
		t.Errorf("unread = %v; the identical copy in a folder a command named was read", got)
	}
	// Nothing of it read: the attached price list is unread.
	st.Commands = nil
	if got := unreadSources(st); len(got) != 1 || got[0] != filepath.Join(inbox, "price_list_Oct2026.xlsx") {
		t.Errorf("unread = %v; want the attached price list", got)
	}
}

func TestAValueTracedInWhatWasReadIsNotHeldBackForAnUnreadFile(t *testing.T) {
	st := groundingState("Enter the orders. "+attachedFilesMark+"\n  /home/headless/work/inbox/u/price_list_Oct2026.xlsx\n",
		"cat '/home/headless/work/P2/WhatsApp Chat with Arjun Rao.txt'")
	st.noteSeen("18/10/2026, 09:12 - Arjun: 60 mugs please")
	if n := groundingNudge(st, `gui type e7 "10/18/2026"`); n != "" {
		t.Errorf("a date the chat gave was refused: %q", n)
	}
	// A value traced nowhere, with the source unread: read it first - the person's question names it plainly.
	nudge, values := groundingCheck(st, `gui type e7 "INV-77812"`)
	q := untracedQuestion(values, nudge)
	if !strings.Contains(nudge, "office read /home/headless/work/inbox/u/price_list_Oct2026.xlsx") {
		t.Errorf("nudge = %q", nudge)
	}
	if !strings.Contains(q, "could not read price_list_Oct2026.xlsx") || strings.Contains(q, "/home/headless") {
		t.Errorf("question = %q", q)
	}
}
