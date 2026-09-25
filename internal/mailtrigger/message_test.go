package mailtrigger

import (
	"strings"
	"testing"
)

func TestParseHeaderDecodesSubjectAndSender(t *testing.T) {
	header := "From: =?UTF-8?Q?Jos=C3=A9_P=C3=A9rez?= <Jose@Example.COM>\r\n" +
		"Subject: =?ISO-8859-1?Q?Factura_n=BA_42?=\r\n" +
		"Date: Thu, 24 Sep 2026 09:15:00 +0200\r\n" +
		"Message-ID: <abc@example.com>\r\n\r\n"

	msg, err := parseHeader(1, []byte(header), "imap.gmail.com")
	if err != nil {
		t.Fatal(err)
	}

	if msg.Sender != "jose@example.com" {
		t.Errorf("Sender = %q", msg.Sender)
	}

	if msg.From != "José Pérez <Jose@Example.COM>" {
		t.Errorf("From = %q", msg.From)
	}

	if msg.Subject != "Factura nº 42" {
		t.Errorf("Subject = %q", msg.Subject)
	}

	if msg.Received.Hour() != 7 {
		t.Errorf("Received = %v, want 07:15 UTC", msg.Received)
	}
}

func TestVerifySenderTrustsOnlyTheProvidersOwnVerdict(t *testing.T) {
	genuine := "Authentication-Results: mx.google.com;\r\n" +
		"       dkim=pass header.i=@billing.acme.com header.s=s1;\r\n" +
		"       spf=pass smtp.mailfrom=acme.com;\r\n" +
		"       dmarc=pass (p=REJECT sp=REJECT dis=NONE) header.from=acme.com\r\n" +
		"From: Billing <invoices@acme.com>\r\nSubject: Invoice\r\n\r\n"

	msg, err := parseHeader(1, []byte(genuine), "imap.gmail.com")
	if err != nil {
		t.Fatal(err)
	}

	if !msg.Verified {
		t.Fatalf("a DMARC pass for the sender's domain was not believed: %s", msg.VerifyNote)
	}

	// A spoofer writes a passing header into its own email. Gmail puts its
	// real verdict on top of it.
	spoofed := "Authentication-Results: mx.google.com;\r\n" +
		"       spf=softfail smtp.mailfrom=evil.example;\r\n" +
		"       dmarc=fail (p=NONE) header.from=acme.com\r\n" +
		"Authentication-Results: mx.google.com; dmarc=pass header.from=acme.com\r\n" +
		"From: Billing <invoices@acme.com>\r\nSubject: Invoice\r\n\r\n"

	msg, _ = parseHeader(2, []byte(spoofed), "imap.gmail.com")
	if msg.Verified {
		t.Error("a forged header below the provider's own was believed")
	}

	// With no provider header at all, a sender's own one is not the provider's.
	forgedOnly := "Authentication-Results: evil.example; dmarc=pass header.from=acme.com\r\n" +
		"From: invoices@acme.com\r\n\r\n"

	msg, _ = parseHeader(3, []byte(forgedOnly), "imap.gmail.com")
	if msg.Verified {
		t.Error("an Authentication-Results from another server was believed")
	}

	// A pass for another domain proves nothing about this one.
	other := "Authentication-Results: mx.google.com; dkim=pass header.i=@mailer.example; dmarc=pass header.from=mailer.example\r\n" +
		"From: invoices@acme.com\r\n\r\n"

	msg, _ = parseHeader(4, []byte(other), "imap.gmail.com")
	if msg.Verified {
		t.Error("a pass for another domain was believed")
	}
}

func TestBodyTextPrefersPlainAndDecodes(t *testing.T) {
	header := "From: a@example.com\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n"

	body := "--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=\"inner\"\r\n\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Please pay invoice =E2=82=AC120 by Friday.=\r\nThanks\r\n" +
		"--inner\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<p>Please pay</p>\r\n" +
		"--inner--\r\n" +
		"--outer\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=inv.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"JVBERi0xLjQK\r\n" +
		"--outer--\r\n"

	got := bodyText([]byte(header), []byte(body), 1000)
	if got != "Please pay invoice €120 by Friday.Thanks" {
		t.Errorf("got %q", got)
	}
}

func TestBodyTextFallsBackToHTMLAndSurvivesTruncation(t *testing.T) {
	header := "Content-Type: multipart/alternative; boundary=b\r\n\r\n"
	body := "--b\r\nContent-Type: text/html; charset=iso-8859-1\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"PGh0bWw+PHN0eWxlPnB7fTwvc3R5bGU+PHA+Q2Fm6SAmYW1wOyBjbzwvcD48cD5OZXh0PC9wPjwvaHRtbD4=\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nthis part was cut off by the fetch li"

	got := bodyText([]byte(header), []byte(body), 1000)
	if !strings.Contains(got, "this part was cut off") {
		// The plain part comes second and is truncated: it is still preferred.
		t.Errorf("got %q", got)
	}

	onlyHTML := "--b\r\nContent-Type: text/html; charset=iso-8859-1\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"PGh0bWw+PHN0eWxlPnB7fTwvc3R5bGU+PHA+Q2Fm6SAmYW1wOyBjbzwvcD48cD5OZXh0PC9wPjwvaHRtbD4=\r\n--b--\r\n"

	if got := bodyText([]byte(header), []byte(onlyHTML), 1000); got != "Café & co\nNext" {
		t.Errorf("html fallback = %q", got)
	}

	if got := bodyText([]byte("Content-Type: text/plain\r\n\r\n"), []byte(strings.Repeat("a", 50)), 10); !strings.HasPrefix(got, "aaaaaaaaaa\n[") {
		t.Errorf("truncation = %q", got)
	}
}
