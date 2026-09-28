// Package wsconn is a WebSocket connection (RFC 6455), both ends: the server side the host-app viewer and the phone
// link are served over, and the client side the phone helper dials out with. Only what they need of the protocol:
// the handshake, whole messages in either direction, and ping/pong/close. It is here instead of a library because
// the binary is built static and dependency-light on purpose. Depends on nothing internal.
package wsconn

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Message types.
const (
	OpText   = 0x1
	OpBinary = 0x2
	OpClose  = 0x8
	OpPing   = 0x9
	OpPong   = 0xA
)

const (
	// maxMessage bounds what the other end may send in one message: a paste is at most 200,000 characters, a
	// phone's key frame a few hundred kilobytes.
	maxMessage = 4 << 20
	writeWait  = 10 * time.Second
)

// Conn is one WebSocket connection. A client masks what it sends and reads unmasked frames; a server the reverse.
type Conn struct {
	conn   net.Conn
	br     *bufio.Reader
	wmu    sync.Mutex
	client bool
}

// Upgrade completes the server side of the handshake. A page from another origin cannot open one: the API it would reach lets a
// caller see and drive a program on the host, and a browser sends the page's origin with every WebSocket request.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
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
	conn.SetWriteDeadline(time.Now().Add(writeWait))
	_, err = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
		acceptKey(key)+"\r\n\r\n")
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})
	return &Conn{conn: conn, br: brw.Reader}, nil
}

// acceptKey is the Sec-WebSocket-Accept answer to key.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
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

// Write sends one message. Safe to call from several goroutines.
func (c *Conn) Write(op byte, payload []byte) error {
	var head [14]byte
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
	if c.client {
		// A client masks every frame (RFC 6455 5.3); the key only has to be unpredictable to the page, not secret.
		head[1] |= 0x80
		var mask [4]byte
		_, _ = rand.Read(mask[:])
		copy(head[n:], mask[:])
		n += 4
		masked := make([]byte, len(payload))
		for i, b := range payload {
			masked[i] = b ^ mask[i&3]
		}
		payload = masked
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	if _, err := c.conn.Write(head[:n]); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

// Read returns the next text or binary message, answering pings and reporting a close as io.EOF. Messages from a client
// are always masked, and from a server never; anything else is a protocol error.
func (c *Conn) Read(idle time.Duration) (op byte, msg []byte, err error) {
	var buf []byte
	var first byte
	for {
		c.conn.SetReadDeadline(time.Now().Add(idle))
		var h [2]byte
		if _, err = io.ReadFull(c.br, h[:]); err != nil {
			return 0, nil, err
		}
		fin, code, isMasked := h[0]&0x80 != 0, h[0]&0x0F, h[1]&0x80 != 0
		if !c.client && !isMasked {
			return 0, nil, errors.New("unmasked client frame")
		}
		if c.client && isMasked {
			return 0, nil, errors.New("masked server frame")
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
		if length > maxMessage || uint64(len(buf))+length > maxMessage {
			return 0, nil, errors.New("websocket message too large")
		}
		var mask [4]byte
		if isMasked {
			if _, err = io.ReadFull(c.br, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, length)
		if _, err = io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		if isMasked {
			for i := range payload {
				payload[i] ^= mask[i&3]
			}
		}

		switch code {
		case OpPing:
			c.Write(OpPong, payload)
			continue
		case OpPong:
			continue
		case OpClose:
			c.Write(OpClose, payload)
			return 0, nil, io.EOF
		case 0x0: // continuation
			if first == 0 {
				return 0, nil, errors.New("unexpected continuation frame")
			}
		case OpText, OpBinary:
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

// Close closes the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Dial opens a client connection to rawURL - ws:// or wss://, or the http(s):// address of the same page - sending
// header with the handshake (a cookie, say). A server that answers anything but a correct 101 is an error that says
// what it answered, since that is usually a login page or a 403 a person needs to read.
func Dial(ctx context.Context, rawURL string, header http.Header) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	secure := false
	switch u.Scheme {
	case "wss", "https":
		secure = true
	case "ws", "http":
	default:
		return nil, fmt.Errorf("wsconn: cannot dial a %q address", u.Scheme)
	}

	host := u.Host
	if u.Port() == "" {
		if secure {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}

	if secure {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tc
	}

	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(raw[:])

	req := &http.Request{
		Method:     http.MethodGet,
		URL:        &url.URL{Path: u.Path, RawQuery: u.RawQuery},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Host:       u.Host,
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("wsconn: %s answered %s: %s", u.Host, resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		conn.Close()
		return nil, errors.New("wsconn: the server's handshake answer does not match")
	}
	conn.SetDeadline(time.Time{})

	return &Conn{conn: conn, br: br, client: true}, nil
}
