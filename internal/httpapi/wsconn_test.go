package httpapi

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// wsPair returns the server's end of a connection and the raw client end, so a test can write exactly the bytes it wants.
func wsPair(t *testing.T) (*wsConn, net.Conn) {
	t.Helper()
	srv, cli := net.Pipe()
	t.Cleanup(func() { srv.Close(); cli.Close() })
	return &wsConn{conn: srv, br: bufio.NewReader(srv)}, cli
}

// masked builds a client frame.
func masked(fin bool, op byte, payload []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	f := []byte{b0}
	switch l := len(payload); {
	case l < 126:
		f = append(f, 0x80|byte(l))
	case l < 1<<16:
		f = append(f, 0x80|126, byte(l>>8), byte(l))
	default:
		f = append(f, 0x80|127)
		f = binary.BigEndian.AppendUint64(f, uint64(l))
	}
	mask := []byte{9, 8, 7, 6}
	f = append(f, mask...)
	for i, b := range payload {
		f = append(f, b^mask[i%4])
	}
	return f
}

type wsRead struct {
	op  byte
	msg []byte
	err error
}

func readAsync(c *wsConn) chan wsRead {
	ch := make(chan wsRead, 1)
	go func() {
		op, msg, err := c.read(2 * time.Second)
		ch <- wsRead{op, msg, err}
	}()
	return ch
}

func TestWSReadsMessagesOfEveryLengthEncoding(t *testing.T) {
	for _, n := range []int{0, 5, 125, 126, 300, 65535, 65536, 70000} {
		c, cli := wsPair(t)
		want := bytes.Repeat([]byte("x"), n)
		res := readAsync(c)
		if _, err := cli.Write(masked(true, wsText, want)); err != nil {
			t.Fatal(err)
		}
		r := <-res
		if r.err != nil || r.op != wsText || !bytes.Equal(r.msg, want) {
			t.Fatalf("%d bytes: op %d, %d bytes back, err %v", n, r.op, len(r.msg), r.err)
		}
	}
}

func TestWSJoinsAFragmentedMessage(t *testing.T) {
	c, cli := wsPair(t)
	res := readAsync(c)
	cli.Write(masked(false, wsText, []byte("hel")))
	cli.Write(masked(true, 0x0, []byte("lo")))
	if r := <-res; r.err != nil || r.op != wsText || string(r.msg) != "hello" {
		t.Fatalf("got op %d %q %v", r.op, r.msg, r.err)
	}
}

func TestWSAnswersAPingAndKeepsReading(t *testing.T) {
	c, cli := wsPair(t)
	res := readAsync(c)
	go cli.Write(masked(true, wsPing, []byte("hi")))
	// The pong comes back on the wire...
	var h [2]byte
	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(cli, h[:]); err != nil || h[0] != 0x80|wsPong || h[1] != 2 {
		t.Fatalf("pong header %v, %v", h, err)
	}
	body := make([]byte, 2)
	io.ReadFull(cli, body)
	if string(body) != "hi" {
		t.Fatalf("pong body %q, want the ping's", body)
	}
	// ...and the message after it is still delivered.
	cli.Write(masked(true, wsText, []byte("after")))
	if r := <-res; r.err != nil || string(r.msg) != "after" {
		t.Fatalf("got %q %v", r.msg, r.err)
	}
}

func TestWSReportsACloseAsEOF(t *testing.T) {
	c, cli := wsPair(t)
	res := readAsync(c)
	go cli.Write(masked(true, wsClose, []byte{0x03, 0xe8}))
	go io.Copy(io.Discard, cli) // the close is answered
	if r := <-res; r.err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", r.err)
	}
}

func TestWSRefusesWhatABrowserWouldNeverSend(t *testing.T) {
	// A client frame that is not masked.
	c, cli := wsPair(t)
	res := readAsync(c)
	go cli.Write([]byte{0x81, 0x02, 'h', 'i'})
	if r := <-res; r.err == nil || !strings.Contains(r.err.Error(), "unmasked") {
		t.Fatalf("an unmasked frame gave %v", r.err)
	}

	// A message over the limit, declared in its header: refused before any of it is read into memory.
	c, cli = wsPair(t)
	res = readAsync(c)
	go cli.Write([]byte{0x82, 0x80 | 127, 0, 0, 0, 0, 0x10, 0, 0, 0}) // 256 MiB
	if r := <-res; r.err == nil || !strings.Contains(r.err.Error(), "too large") {
		t.Fatalf("an oversized message gave %v", r.err)
	}

	// A continuation with nothing to continue.
	c, cli = wsPair(t)
	res = readAsync(c)
	go cli.Write(masked(true, 0x0, []byte("x")))
	if r := <-res; r.err == nil {
		t.Fatal("a stray continuation frame was accepted")
	}
}

func TestWSWritesLargeMessagesWithTheRightLengthEncoding(t *testing.T) {
	for _, n := range []int{0, 125, 126, 65535, 65536, 200000} {
		c, cli := wsPair(t)
		payload := bytes.Repeat([]byte{7}, n)
		go c.write(wsBinary, payload)
		var h [2]byte
		cli.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(cli, h[:]); err != nil {
			t.Fatal(err)
		}
		if h[0] != 0x80|wsBinary || h[1]&0x80 != 0 {
			t.Fatalf("%d bytes: header %v, want a final unmasked binary frame", n, h)
		}
		got := uint64(h[1] & 0x7F)
		switch got {
		case 126:
			var b [2]byte
			io.ReadFull(cli, b[:])
			got = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			io.ReadFull(cli, b[:])
			got = binary.BigEndian.Uint64(b[:])
		}
		if int(got) != n {
			t.Fatalf("declared length %d, want %d", got, n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(cli, body); err != nil || !bytes.Equal(body, payload) {
			t.Fatalf("%d bytes: body differs (%v)", n, err)
		}
	}
}
