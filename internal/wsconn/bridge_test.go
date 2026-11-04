package wsconn

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// screen stands in for a desktop's Xvnc: it accepts one connection and hands it to the test.
func screen(t *testing.T) (addr string, conns <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			ch <- c
		}
	}()
	return ln.Addr().String(), ch
}

// bridged is a server that bridges each WebSocket to addr, and reports each bridge's end on ended.
func bridged(t *testing.T, addr string) (url string, ended <-chan error) {
	t.Helper()
	ch := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tcp, err := net.Dial("tcp", addr)
		if err != nil {
			http.Error(w, "no screen", http.StatusBadGateway)
			return
		}
		c, err := UpgradeGuarded(w, r)
		if err != nil {
			tcp.Close()
			return
		}
		ch <- Bridge(c, tcp, 50*time.Millisecond, 2*time.Second)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/desktop/box/websockify", ch
}

// The bridge carries a screen's bytes both ways unchanged - the browser's messages to the screen, the screen's
// bytes to the browser as binary messages - and keeps the WebSocket alive with pings (Phase 2 C).
func TestTheScreenBridgeCarriesBytesBothWays(t *testing.T) {
	addr, conns := screen(t)
	url, _ := bridged(t, addr)
	c, err := Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	vnc := <-conns
	defer vnc.Close()

	if _, err := vnc.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	op, msg, err := c.Read(time.Second)
	if err != nil || op != OpBinary || string(msg) != "RFB 003.008\n" {
		t.Fatalf("the browser got %d %q %v", op, msg, err)
	}
	if err := c.Write(OpBinary, []byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 12)
	_ = vnc.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(vnc, got); err != nil || string(got) != "RFB 003.008\n" {
		t.Fatalf("the screen got %q %v", got, err)
	}
	// A large screen update arrives whole, however many messages carry it.
	big := bytes.Repeat([]byte{7}, 300<<10)
	go vnc.Write(big)
	var all []byte
	for len(all) < len(big) {
		_, part, err := c.Read(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, part...)
	}
	if !bytes.Equal(all, big) {
		t.Fatal("a large update arrived changed")
	}
}

// The screen ending closes the WebSocket with a normal close and the bridge ends cleanly; the browser closing
// closes the screen's connection and the bridge ends cleanly.
func TestTheScreenBridgeClosesCleanly(t *testing.T) {
	t.Run("the screen ends", func(t *testing.T) {
		addr, conns := screen(t)
		url, ended := bridged(t, addr)
		c, err := Dial(context.Background(), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		(<-conns).Close()
		if _, _, err := c.Read(time.Second); !errors.Is(err, io.EOF) {
			t.Fatalf("the browser read %v, want a close", err)
		}
		if err := <-ended; err != nil {
			t.Fatalf("the bridge ended with %v", err)
		}
	})
	t.Run("the browser closes", func(t *testing.T) {
		addr, conns := screen(t)
		url, ended := bridged(t, addr)
		c, err := Dial(context.Background(), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		vnc := <-conns
		defer vnc.Close()
		if err := c.Write(OpClose, []byte{0x03, 0xe8}); err != nil {
			t.Fatal(err)
		}
		_ = vnc.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := vnc.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("the screen read %v, want its connection closed", err)
		}
		if err := <-ended; err != nil {
			t.Fatalf("the bridge ended with %v", err)
		}
		c.Close()
	})
}

// The guarded upgrade answers the "binary" subprotocol when it is offered (websockify did), and makes no origin
// check of its own (its listener's guard did; behind a tunnel Host and Origin differ).
func TestTheGuardedUpgradeChoosesBinaryAndLeavesOriginToTheGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := UpgradeGuarded(w, r); err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "GET /desktop/box/websockify HTTP/1.1\r\nHost: localhost:8090\r\nOrigin: https://tunnel.example.test\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary, base64\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Protocol") != "binary" {
		t.Fatalf("answered %s with protocol %q", resp.Status, resp.Header.Get("Sec-WebSocket-Protocol"))
	}
}
