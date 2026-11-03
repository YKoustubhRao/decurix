package phoneagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/phonehost"
)

// A saved sign-in is offered only to the control plane it was made for
// (review 2026-11-03, B-18).
func TestASavedTokenGoesOnlyToItsOwnControlPlane(t *testing.T) {
	saved := Saved{Base: "https://a.example.com", Token: "t", Name: "Pixel"}
	for base, want := range map[string]bool{
		"https://a.example.com":  true,
		"https://A.example.com/": true,
		"https://b.example.com":  false,
		"http://a.example.com":   false,
	} {
		if got := SameControlPlane(saved, base); got != want {
			t.Errorf("SameControlPlane(%q) = %v, want %v", base, got, want)
		}
	}
	if SameControlPlane(Saved{Base: "https://a.example.com"}, "https://a.example.com") {
		t.Error("an empty token counts as a pairing")
	}
}

// A password field's text never reaches the control plane, even when Android
// shows it in the dump (review 2026-11-03, B-22).
func TestAPasswordFieldsTextIsNotSent(t *testing.T) {
	dump := strings.Replace(sampleDump, `text="" resource-id="" class="android.widget.EditText" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="true" password="true"`,
		`text="4711" resource-id="" class="android.widget.EditText" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="true" password="true"`, 1)
	if dump == sampleDump {
		t.Fatal("the sample's password field was not found")
	}
	d, err := parseDump([]byte(dump))
	if err != nil {
		t.Fatal(err)
	}
	tree := treeFor(d, 12, "com.whatsapp", 720, 1520)
	found := false
	for _, n := range tree.Nodes {
		if n.Password {
			found = true
			if n.Value != "" {
				t.Fatalf("a password field's text was sent: %+v", n)
			}
		}
	}
	if !found {
		t.Fatal("no password field in the tree")
	}
}

// Setting up ADB never opens adb's legacy network port (review 2026-11-03,
// B-26): it stays on Wireless debugging's own paired connection.
func TestConnectingADBNeverOpensTheLegacyPort(t *testing.T) {
	var calls [][]string
	defer func(r func(context.Context, ...string) ([]byte, error), p func(context.Context, string) (string, error)) {
		runADB, pickSerial = r, p
	}(runADB, pickSerial)
	runADB = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("connected"), nil
	}
	pickSerial = func(_ context.Context, want string) (string, error) { return want, nil }

	serial, err := connectWireless(context.Background(), "localhost:41234")
	if err != nil || serial != "localhost:41234" {
		t.Fatalf("serial %q, %v", serial, err)
	}
	for _, c := range calls {
		if strings.Join(c, " ") != "connect localhost:41234" {
			t.Fatalf("adb was asked to %v", c)
		}
	}
}

// A video frame larger than one link message can carry ends its session, not
// the whole link (review 2026-11-03, B-37).
func TestAFrameTooLargeForTheLinkIsRefused(t *testing.T) {
	head := make([]byte, frameHeaderLength)
	binary.BigEndian.PutUint32(head[8:], maxVideoPacket+1)
	if _, err := readFrame(bytes.NewReader(head)); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if frameHeaderLength+maxVideoPacket+4 != 4<<20 {
		t.Fatal("the largest frame does not fit one link message exactly")
	}
}

// Text that is not UTF-8 still splits, and the split ends (B-37).
func TestSplitTextEndsOnBytesThatAreNotUTF8(t *testing.T) {
	done := make(chan []string, 1)
	go func() { done <- splitText(strings.Repeat("\x80", 400), maxInjectText) }()
	select {
	case parts := <-done:
		if strings.Join(parts, "") != strings.Repeat("\x80", 400) {
			t.Fatal("bytes were lost")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("splitText did not return")
	}
}

// The phone's own encoder masks a key's meta state too, whoever sent it (RW1-B-06).
func TestTheEncoderSendsOnlyTypingModifiers(t *testing.T) {
	out, err := encodeInput(phonehost.Input{Kind: "key", Key: 29, Meta: 0x1 | 0x10000}) // A, with SHIFT and META
	if err != nil || len(out) != 1 {
		t.Fatalf("%v %v", out, err)
	}
	if got := binary.BigEndian.Uint32(out[0][10:]); got != 0x1 {
		t.Fatalf("meta sent = %#x, want shift only", got)
	}
}
