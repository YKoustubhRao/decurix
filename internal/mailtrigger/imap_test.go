package mailtrigger

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

// fakeIMAPServer answers each command the client sends with the scripted
// reply for its verb, echoing the client's own tag, and records the commands.
func fakeIMAPServer(t *testing.T, replies map[string]string) (*imapClient, *[]string) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	var seen []string

	go func() {
		r := bufio.NewReader(server)
		fmt.Fprint(server, "* OK IMAP4rev1 ready\r\n")

		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimRight(line, "\r\n")
			seen = append(seen, line)

			tag, rest, _ := strings.Cut(line, " ")
			verb := strings.ToUpper(strings.Fields(rest)[0])
			if verb == "UID" {
				verb += " " + strings.ToUpper(strings.Fields(rest)[1])
			}

			reply, ok := replies[verb]
			if !ok {
				reply = "{tag} OK done\r\n"
			}

			fmt.Fprint(server, strings.ReplaceAll(reply, "{tag}", tag))
		}
	}()

	c := newIMAPClient(client)
	if err := c.greeting(); err != nil {
		t.Fatal(err)
	}

	return c, &seen
}

func TestIMAPLoginQuotesAndReportsARefusal(t *testing.T) {
	c, seen := fakeIMAPServer(t, map[string]string{
		"LOGIN": "{tag} NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)\r\n",
	})

	err := c.login("ada@example.com", `pa"ss\word`)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}

	if !strings.Contains(err.Error(), "Invalid credentials") {
		t.Errorf("the server's own words are lost: %v", err)
	}

	if got := (*seen)[0]; !strings.HasSuffix(got, `LOGIN "ada@example.com" "pa\"ss\\word"`) {
		t.Errorf("sent %q", got)
	}
}

func TestIMAPExamineReadsTheMailboxPosition(t *testing.T) {
	c, _ := fakeIMAPServer(t, map[string]string{
		"EXAMINE": "* 172 EXISTS\r\n* OK [UIDVALIDITY 3857529045] UIDs valid\r\n* OK [UIDNEXT 4392] Predicted next UID\r\n{tag} OK [READ-ONLY] done\r\n",
	})

	validity, next, err := c.Examine("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	if validity != 3857529045 || next != 4392 {
		t.Errorf("validity %d next %d", validity, next)
	}
}

func TestIMAPSearchDropsTheHighestUIDWhenItIsNotNew(t *testing.T) {
	// "UID SEARCH UID 101:*" on a mailbox whose highest UID is 100 answers 100.
	c, _ := fakeIMAPServer(t, map[string]string{
		"UID SEARCH": "* SEARCH 100\r\n{tag} OK done\r\n",
	})

	uids, err := c.UIDsAfter(100)
	if err != nil {
		t.Fatal(err)
	}

	if len(uids) != 0 {
		t.Errorf("uids = %v, want none", uids)
	}
}

func TestIMAPFetchFollowsLiteralsAndSkipsUnsolicitedFlags(t *testing.T) {
	h1 := "From: a@example.com\r\nSubject: One\r\n\r\n"
	h2 := "From: b@example.com\r\nSubject: Two {3}\r\n\r\n"

	c, _ := fakeIMAPServer(t, map[string]string{
		"UID FETCH": fmt.Sprintf("* 1 FETCH (FLAGS (\\Seen))\r\n* 2 FETCH (UID 7 BODY[HEADER] {%d}\r\n%s)\r\n* 3 FETCH (BODY[HEADER] {%d}\r\n%s UID 9)\r\n{tag} OK done\r\n",
			len(h1), h1, len(h2), h2),
	})

	got, err := c.Headers([]uint32{7, 9})
	if err != nil {
		t.Fatal(err)
	}

	if string(got[7]) != h1 || string(got[9]) != h2 {
		t.Errorf("got %q and %q", got[7], got[9])
	}
}
