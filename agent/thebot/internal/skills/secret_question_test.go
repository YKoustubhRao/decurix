package skills

import (
	"strings"
	"testing"
)

// Round 5 P2 (2026-10-20): the request gives the portal's sign-in in its own words; a PO .eml it also names was not
// read yet. The sign-in was refused, and the refusal's question showed the password in the chat.
const p2Request = "Morning! Today's orders please.\n\nPlease:\n1. Enter every approved order in the Northwind Back Office " +
	"portal (http://host.docker.internal:8701, user clerk, password Northwind#2026): one order per customer PO or chat.\n\n" +
	"Files attached to this request, saved on this desktop:\n  /home/headless/work/inbox/P2/PO_HH-PO-7781.eml\n"

func p2State() *operatorState {
	st := groundingState(p2Request)
	st.noteSeen("Screen 1280x720. Active window: \"Sign in - Chromium\"\ne4 textbox \"Username\"\ne5 password text \"Password\"\ne6 button \"Sign in\"")
	return st
}

func TestTheSignInTheRequestGivesIsTypedBeforeItsDocumentsAreRead(t *testing.T) {
	st := p2State()
	for _, cmd := range []string{`gui type e4 "clerk"`, `gui type e5 'Northwind#2026'`,
		`gui type e4 "clerk"; gui type e5 'Northwind#2026'; gui click e6`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused: %q", cmd, n)
		}
	}
	// A value the request does not give still waits for the document.
	if n := groundingNudge(st, `gui type e9 "Harbour Hotels Group"`); !strings.Contains(n, "you have not read it") {
		t.Errorf("a value only the unread PO gives was let through: %q", n)
	}
}

func TestARefusalNeverShowsASecret(t *testing.T) {
	st := p2State()
	// A password typed with a value the PO gives: refused for the PO, and the question names the field, not the value.
	nudge, values := groundingCheck(st, `gui type e5 'Northwind#2026'; gui type e9 "HH-PO-7781-X"`)
	q := untracedQuestion(values, nudge)
	if strings.Contains(q, "Northwind#2026") || strings.Contains(nudge, "Northwind#2026") {
		t.Fatalf("a secret reached the question or the nudge: %q / %q", q, nudge)
	}

	// A password the request does not give, typed into the password field: refused as untraced, never shown.
	st = p2State()
	st.Request = strings.Replace(st.Request, "/home/headless/work/inbox/P2/PO_HH-PO-7781.eml", "", 1)
	st.Commands = append(st.Commands, "office read /home/headless/work/inbox/P2/PO_HH-PO-7781.eml")
	nudge, values = groundingCheck(st, `gui type e5 'Hunter2-guess'`)
	q = untracedQuestion(values, nudge)
	if nudge == "" {
		t.Fatal("a guessed password was let through")
	}
	if strings.Contains(q, "Hunter2-guess") || strings.Contains(nudge, "Hunter2-guess") {
		t.Errorf("a password typed into a password field was shown: %q / %q", q, nudge)
	}
	if !strings.Contains(q, secretShown) {
		t.Errorf("the question does not say which field: %q", q)
	}
}
