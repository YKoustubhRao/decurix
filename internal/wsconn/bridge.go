package wsconn

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// UpgradeGuarded completes the server side of the handshake for a route whose listener's guard has already held the
// request to its own pages (httpapi.GuardPrimary, GuardGated: Origin against Host, or the tunnel's forwarded host). It
// makes no origin check of its own: behind a tunnel the Host is the tunnel's local target while the page is on its
// public name, which Upgrade's check would refuse though the guard allowed it. The subprotocol "binary" is chosen when
// the client offers it, as websockify did.
func UpgradeGuarded(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHasToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "expected a WebSocket upgrade", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade")
	}
	return handshake(w, r, "binary")
}

// bridgeChunk is the most of a TCP read sent as one WebSocket message.
const bridgeChunk = 64 << 10

// Bridge carries bytes both ways between c and tcp until either side ends, as websockify does for a VNC screen: every
// message from c (text or binary) is written to tcp as it is, and whatever tcp sends goes to c as binary messages.
// The WebSocket is pinged every keepalive (a browser answers, which keeps the read alive and finds a dead peer), and a
// side that says nothing for idle (the pongs included) ends it. When tcp ends, c is sent a normal close; when c
// closes, tcp is closed. Both are closed when it returns.
func Bridge(c *Conn, tcp net.Conn, keepalive, idle time.Duration) error {
	var once sync.Once
	var first error
	end := func(err error) {
		once.Do(func() {
			first = err
			_ = tcp.Close()
			_ = c.Close()
		})
	}
	done := make(chan struct{})

	// TCP to the WebSocket.
	go func() {
		buf := make([]byte, bridgeChunk)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if werr := c.Write(OpBinary, buf[:n]); werr != nil {
					end(werr)
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					// The screen ended: a normal close (1000), then the connection.
					_ = c.Write(OpClose, []byte{0x03, 0xe8})
					end(nil)
					return
				}
				end(err)
				return
			}
		}
	}()

	// Keepalive pings.
	go func() {
		tick := time.NewTicker(keepalive)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if err := c.Write(OpPing, nil); err != nil {
					end(err)
					return
				}
			}
		}
	}()

	// The WebSocket to TCP, on this goroutine.
	for {
		_, msg, err := c.Read(idle)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil // the browser closed: a clean end
			}
			end(err)
			break
		}
		if _, err := tcp.Write(msg); err != nil {
			end(err)
			break
		}
	}
	close(done)
	if first != nil && isClosedConn(first) {
		return nil
	}
	return first
}

// isClosedConn reports whether err is only the other half of an end already under way (a read on a closed socket).
func isClosedConn(err error) bool {
	return errors.Is(err, net.ErrClosed)
}
