package mailtrigger

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// An attachment whose "extension" is longer than the name limit crashed the control plane in a loop.
func TestSafeFileNameSurvivesALongExtension(t *testing.T) {
	for _, name := range []string{
		"a." + strings.Repeat("x", 200),
		strings.Repeat("é", 120) + ".pdf",
		strings.Repeat("b", 300) + ".xlsx",
	} {
		got := SafeFileName(name)
		if len(got) > 150 || got == "" || !utf8.ValidString(got) {
			t.Errorf("SafeFileName(%d bytes) = %q", len(name), got)
		}
	}
	if got := SafeFileName(strings.Repeat("b", 300) + ".xlsx"); !strings.HasSuffix(got, ".xlsx") {
		t.Errorf("a real extension was lost: %q", got)
	}
}

// A display name that looks like an address is not the address.
func TestSenderRuleMatchesTheAddressNotTheDisplayName(t *testing.T) {
	rules := compileRules([]Rule{{ID: "r", Enabled: true, SenderRe: "billing@acme.com"}})
	spoof := Message{From: `"billing@acme.com" <x@evil.com>`, Sender: "x@evil.com"}
	if rules[0].matches(spoof) {
		t.Error("a display name passed a rule for an address")
	}
	if !rules[0].matches(Message{From: "Billing <billing@acme.com>", Sender: "billing@acme.com"}) {
		t.Error("the real sender did not match")
	}
	byName := compileRules([]Rule{{ID: "n", Enabled: true, SenderRe: "Ada Lovelace"}})
	if !byName[0].matches(Message{From: "Ada Lovelace <ada@example.com>", Sender: "ada@example.com"}) {
		t.Error("a rule for a name no longer matches the name")
	}
	verified := compileRules([]Rule{{ID: "v", Enabled: true, SenderRe: "Ada Lovelace", VerifiedOnly: true}})
	if verified[0].matches(Message{From: "Ada Lovelace <x@evil.com>", Sender: "x@evil.com"}) {
		t.Error("a verified-only rule matched a display name")
	}
}

// A provider whose own check cannot be told from a forged header confirms nothing.
func TestUnknownProviderVerifiesNoSender(t *testing.T) {
	header := []byte("From: Billing <billing@acme.com>\r\nAuthentication-Results: mx.attacker; dmarc=pass header.from=acme.com\r\nSubject: hi\r\n\r\n")
	msg, err := parseHeader(1, header, "imap.unknown-provider.example")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Verified {
		t.Errorf("a forged Authentication-Results was believed: %s", msg.VerifyNote)
	}
}
