package phoneagent

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/phonehost"
)

// appListOutput is list_apps output in the shape util/LogUtils.buildAppListMessage (v4.1) writes it.
const appListOutput = `[server] INFO: Device: [Google] google Pixel 8 (Android 15)
[server] INFO: List of apps:
 * Camera                         com.android.camera
 * Settings                       com.android.settings
 - Foo v1.2                       org.example.foo
 - WhatsApp                       com.whatsapp
 - An application with a very long name
                                com.example.longname
`

func TestParseAppList(t *testing.T) {
	got := parseAppList(appListOutput)
	want := []phonehost.App{
		{Name: "Camera", Package: "com.android.camera", System: true},
		{Name: "Settings", Package: "com.android.settings", System: true},
		{Name: "Foo v1.2", Package: "org.example.foo"},
		{Name: "WhatsApp", Package: "com.whatsapp"},
		{Name: "An application with a very long name", Package: "com.example.longname"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d apps %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("app %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The layouts below are scrcpy v4.1's app/src/control_msg.c, byte for byte.
func TestTouchIsThirtyTwoBytesAsScrcpyWritesIt(t *testing.T) {
	msgs, err := encodeInput(phonehost.Input{Kind: "touch", Action: 0, X: 100, Y: 200, W: 720, H: 1440})
	if err != nil || len(msgs) != 1 {
		t.Fatal(msgs, err)
	}

	b := msgs[0]
	if len(b) != 32 || b[0] != 2 || b[1] != 0 {
		t.Fatalf("header %v", b[:2])
	}
	if binary.BigEndian.Uint64(b[2:]) != ^uint64(1) { // UINT64_C(-2), a generic finger
		t.Errorf("pointer id %x", binary.BigEndian.Uint64(b[2:]))
	}
	if binary.BigEndian.Uint32(b[10:]) != 100 || binary.BigEndian.Uint32(b[14:]) != 200 ||
		binary.BigEndian.Uint16(b[18:]) != 720 || binary.BigEndian.Uint16(b[20:]) != 1440 {
		t.Errorf("position %v", b[10:22])
	}
	if binary.BigEndian.Uint16(b[22:]) != 0xffff {
		t.Errorf("a finger down has pressure %x", binary.BigEndian.Uint16(b[22:]))
	}

	up, _ := encodeInput(phonehost.Input{Kind: "touch", Action: 1, X: 100, Y: 200, W: 720, H: 1440})
	if binary.BigEndian.Uint16(up[0][22:]) != 0 {
		t.Error("a finger up has pressure")
	}
}

func TestKeyTextScrollBackAndStartApp(t *testing.T) {
	key, _ := encodeInput(phonehost.Input{Kind: "key", Action: 1, Key: 66, Meta: 1})
	if want := []byte{0, 1, 0, 0, 0, 66, 0, 0, 0, 0, 0, 0, 0, 1}; !bytes.Equal(key[0], want) {
		t.Errorf("key = %v, want %v", key[0], want)
	}

	text, _ := encodeInput(phonehost.Input{Kind: "text", Text: "hi"})
	if want := []byte{1, 0, 0, 0, 2, 'h', 'i'}; !bytes.Equal(text[0], want) {
		t.Errorf("text = %v, want %v", text[0], want)
	}

	// Longer than one message allows: split, never inside a character.
	long := bytes.Repeat([]byte("é"), 200) // 400 bytes
	parts, _ := encodeInput(phonehost.Input{Kind: "text", Text: string(long)})
	var joined []byte
	for _, p := range parts {
		n := binary.BigEndian.Uint32(p[1:])
		if n > maxInjectText || int(n) != len(p)-5 {
			t.Fatalf("a part of %d bytes", n)
		}
		joined = append(joined, p[5:]...)
	}
	if !bytes.Equal(joined, long) {
		t.Error("split text does not join back to what was typed")
	}

	scroll, _ := encodeInput(phonehost.Input{Kind: "scroll", X: 1, Y: 2, W: 3, H: 4, ScrollY: -16})
	if len(scroll[0]) != 21 || scroll[0][0] != 3 || int16(binary.BigEndian.Uint16(scroll[0][15:])) != -32768 {
		t.Errorf("scroll = %v", scroll[0])
	}

	back, _ := encodeInput(phonehost.Input{Kind: "back"})
	if len(back) != 2 || !bytes.Equal(back[0], []byte{4, 0}) || !bytes.Equal(back[1], []byte{4, 1}) {
		t.Errorf("back = %v", back)
	}

	if got := startAppMessage("com.whatsapp"); !bytes.Equal(got, append([]byte{16, 12}, "com.whatsapp"...)) {
		t.Errorf("start app = %v", got)
	}
}

func TestReadFrameTakesSessionAndMediaPackets(t *testing.T) {
	var stream bytes.Buffer

	// A session packet: the top bit set, then width and height, no payload.
	session := []byte{0x80, 0, 0, 0, 0, 0, 2, 0xd0, 0, 0, 5, 0xa0}
	stream.Write(session)

	// A key frame of 3 bytes.
	media := make([]byte, 12)
	binary.BigEndian.PutUint64(media, 1<<61|42)
	binary.BigEndian.PutUint32(media[8:], 3)
	stream.Write(media)
	stream.Write([]byte{9, 8, 7})

	got, err := readFrame(&stream)
	if err != nil || !bytes.Equal(got, session) {
		t.Fatalf("session packet: %v %v", got, err)
	}

	got, err = readFrame(&stream)
	if err != nil || !bytes.Equal(got, append(media, 9, 8, 7)) {
		t.Fatalf("media packet: %v %v", got, err)
	}
}

func TestParseLink(t *testing.T) {
	base, code, err := ParseLink("https://a-b.trycloudflare.com/__decurix_access/authenticate?code=Abc234&remember=1&next=%2F")
	if err != nil || base != "https://a-b.trycloudflare.com" || code != "Abc234" {
		t.Fatalf("got %q %q %v", base, code, err)
	}

	for _, bad := range []string{"https://x.com/", "ftp://x/__decurix_access/authenticate?code=a", "not a link"} {
		if _, _, err := ParseLink(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestMDNSLinesAreRead(t *testing.T) {
	out := "List of discovered mdns services\nadb-2A1B-xyz\t_adb-tls-connect._tcp.\t192.168.1.7:41233\nadb-2A1B-xyz\t_adb-tls-pairing._tcp.\t192.168.1.7:37101\n"
	got := map[string]string{}
	for _, m := range mdnsService.FindAllStringSubmatch(out, -1) {
		got[m[1]] = m[2] + ":" + m[3]
	}
	if got["_adb-tls-pairing._tcp"] != "192.168.1.7:37101" || got["_adb-tls-connect._tcp"] != "192.168.1.7:41233" {
		t.Fatalf("got %v", got)
	}
}
