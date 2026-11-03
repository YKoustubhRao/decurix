package mailtrigger

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestAnAttachmentSurvivesBeingSentAndReadBack(t *testing.T) {
	pdf := bytes.Repeat([]byte("%PDF-1.4 binary \x00\xff "), 400)

	raw, err := BuildMessage("ops@acme.test", OutgoingMail{
		To: []string{"buyer@x.test"}, Subject: "PO 4471 – revised",
		Body: "Please find the PO attached.\nThanks",
		Attachments: []Attachment{
			{Name: "PO-4471.pdf", ContentType: "application/pdf", Data: pdf},
			{Name: "../../etc/prices.xlsx", ContentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Data: []byte("PK\x03\x04")},
		},
	}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}

	if !mayHaveAttachments(raw[:bytes.Index(raw, []byte("\r\n\r\n"))+4]) {
		t.Fatal("a multipart/mixed email was not recognised as carrying files")
	}

	files, _ := ExtractAttachments(raw)
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}

	if files[0].Name != "PO-4471.pdf" || !bytes.Equal(files[0].Data, pdf) {
		t.Fatalf("first file = %q, %d bytes", files[0].Name, len(files[0].Data))
	}

	if files[1].Name != "prices.xlsx" {
		t.Fatalf("a path in an attachment's name was kept: %q", files[1].Name)
	}

	if text := bodyText(raw[:bytes.Index(raw, []byte("\r\n\r\n"))+4], raw[bytes.Index(raw, []byte("\r\n\r\n"))+4:], 500); !strings.Contains(text, "PO attached") {
		t.Fatalf("the body text was lost: %q", text)
	}
}

func TestAPlainEmailKeepsItsBody(t *testing.T) {
	raw, err := BuildMessage("ops@acme.test", OutgoingMail{
		To: []string{"buyer@x.test"}, Subject: "PO 4471", Body: "Please confirm the delivery date.\nThanks",
	}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}

	at := bytes.Index(raw, []byte("\r\n\r\n")) + 4
	if text := bodyText(raw[:at], raw[at:], 500); !strings.Contains(text, "confirm the delivery date") {
		t.Fatalf("an email with no files went out without its body: %q", raw)
	}
}

func TestARequestListsWhereTheFilesAre(t *testing.T) {
	out := mustRender(Rule{Content: "Create the sales order"}, Message{From: "a@b.c", Subject: "PO"}, "", InboxDir("mjob_1"),
		[]Attachment{{Name: "PO.pdf"}})

	if !strings.Contains(out, "/home/headless/work/inbox/mjob_1/PO.pdf") {
		t.Fatalf("the request does not say where the file is:\n%s", out)
	}
}

func TestDuplicateNamesAreKeptApart(t *testing.T) {
	got := uniqueNames([]Attachment{{Name: "a.pdf"}, {Name: "A.pdf"}, {Name: "b.pdf"}})
	if got[1].Name != "A (2).pdf" {
		t.Fatalf("names = %v", got)
	}
}

func TestASkillRuleCarriesTheEmailAndItsFilesAfterItsCommand(t *testing.T) {
	rule := Rule{Content: "/skill enter-a-customer-po  warehouse=\"Main\"", IncludeBody: true}
	out := mustRender(rule, Message{From: "buyer@x.test", Subject: "PO 7731"}, "Please supply...", InboxDir("mjob_9"),
		[]Attachment{{Name: "PO-7731.pdf"}})

	first, rest, _ := strings.Cut(out, "\n")
	if first != `/skill enter-a-customer-po warehouse="Main"` {
		t.Fatalf("the command line changed: %q", first)
	}
	if !strings.Contains(rest, "/home/headless/work/inbox/mjob_9/PO-7731.pdf") || !strings.Contains(rest, "Please supply") {
		t.Fatalf("the email and its files are not after the command:\n%s", out)
	}
	if strings.Contains(rest, "Use the email above to do this") {
		t.Fatalf("a skill run is not asked to do its own command again:\n%s", out)
	}
}

func TestOtherSlashCommandsAreSentAsWritten(t *testing.T) {
	for _, cmd := range []string{"/replay demo-1", "/learn demo-1", "/record Enter a PO"} {
		if out := mustRender(Rule{Content: cmd, IncludeBody: true}, Message{Subject: "x"}, "body", "/d", []Attachment{{Name: "a.pdf"}}); out != cmd {
			t.Fatalf("%s became %q", cmd, out)
		}
	}
}
