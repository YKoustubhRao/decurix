package httpapi

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A WebSocket server connection, and only what the host-app viewer needs of the protocol (RFC 6455): the handshake,
// whole messages in either direction, and ping/pong/close. It is here instead of a library because the binary is
// built static and dependency-light on purpose, and this is about a hundred lines.

const (
	wsText   = 0x1
	wsBinary = 0x2
	wsClose  = 0x8
	wsPing   = 0x9
	wsPong   = 0xA

	// wsMaxMessage bounds what a client may send in one message: a paste is at most 200,000 characters.
	wsMaxMessage = 4 << 20
	wsWriteWait  = 10 * time.Second
)

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// upgradeWebSocket completes the handshake. A page from another origin cannot open one: the API it would reach lets a
// caller see and drive a program on the host, and a browser sends the page's origin with every WebSocket request.
func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHasToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "expected a WebSocket upgrade", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "cross-origin WebSocket refused", http.StatusForbidden)
			return nil, errors.New("cross-origin websocket")
		}
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "bad WebSocket handshake", http.StatusBadRequest)
		return nil, errors.New("bad websocket handshake")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket unsupported", http.StatusInternalServerError)
		return nil, errors.New("response writer cannot be hijacked")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	_, err = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
		base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})
	return &wsConn{conn: conn, br: brw.Reader}, nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// write sends one message. Safe to call from several goroutines.
func (c *wsConn) write(op byte, payload []byte) error {
	var head [10]byte
	head[0] = 0x80 | op
	n := 2
	switch l := len(payload); {
	case l < 126:
		head[1] = byte(l)
	case l < 1<<16:
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:], uint16(l))
		n = 4
	default:
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:], uint64(l))
		n = 10
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if _, err := c.conn.Write(head[:n]); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

// read returns the next text or binary message, answering pings and reporting a close as io.EOF. Messages from a client
// are always masked; one that is not is a protocol error.
func (c *wsConn) read(idle time.Duration) (op byte, msg []byte, err error) {
	var buf []byte
	var first byte
	for {
		c.conn.SetReadDeadline(time.Now().Add(idle))
		var h [2]byte
		if _, err = io.ReadFull(c.br, h[:]); err != nil {
			return 0, nil, err
		}
		fin, code := h[0]&0x80 != 0, h[0]&0x0F
		if h[1]&0x80 == 0 {
			return 0, nil, errors.New("unmasked client frame")
		}
		length := uint64(h[1] & 0x7F)
		switch length {
		case 126:
			var b [2]byte
			if _, err = io.ReadFull(c.br, b[:]); err != nil {
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err = io.ReadFull(c.br, b[:]); err != nil {
				return 0, nil, err
			}
			length = binary.BigEndian.Uint64(b[:])
		}
		if length > wsMaxMessage || uint64(len(buf))+length > wsMaxMessage {
			return 0, nil, errors.New("websocket message too large")
		}
		var mask [4]byte
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
		payload := make([]byte, length)
		if _, err = io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i&3]
		}

		switch code {
		case wsPing:
			c.write(wsPong, payload)
			continue
		case wsPong:
			continue
		case wsClose:
			c.write(wsClose, payload)
			return 0, nil, io.EOF
		case 0x0: // continuation
			if first == 0 {
				return 0, nil, errors.New("unexpected continuation frame")
			}
		case wsText, wsBinary:
			first = code
		default:
			return 0, nil, errors.New("unknown websocket opcode")
		}
		buf = append(buf, payload...)
		if fin {
			return first, buf, nil
		}
	}
}

func (c *wsConn) close() { c.conn.Close() }
