package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/winhost"
	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// A native Windows worker's screen, spoken as VNC.
//
// A desktop container's screen is its Xvnc, reached through websockify and
// shown by the control plane's viewer (web/templates/screen.html, noVNC's RFB
// client). A native worker has no VNC server - its screen is a private desktop
// of this computer, held by winhost's helper - so this is one: the RFB
// protocol over the same WebSocket path (/desktop/<id>/websockify), in front
// of the helper's pictures and input. The viewer, its session bar, clipboard,
// keys, full screen and reconnecting are then the ones every desktop has, with
// nothing in the page that knows which kind it is showing.
//
// Only what noVNC needs is here: protocol 3.8, VNC authentication, pictures as
// Tight-encoding JPEG rectangles (the helper already sends JPEG patches, so
// they are passed on as they are), a cursor, and key, pointer and clipboard
// messages. It is not a VNC server for any other client.

const (
	rfbEncodingTight       = 7
	rfbEncodingCursor      = -239
	rfbEncodingDesktopSize = -223

	// How long the first picture may take: the helper sends one as soon as
	// somebody watches.
	rfbFirstPicture = 15 * time.Second
)

// wsBytes reads a WebSocket's binary messages as one stream of bytes: an RFB
// message may be split across frames, and several may share one.
type wsBytes struct {
	c   *wsconn.Conn
	buf []byte
}

func (w *wsBytes) Read(p []byte) (int, error) {
	for len(w.buf) == 0 {
		op, msg, err := w.c.Read(70 * time.Second) // the browser answers pings, so a quiet line is a dead one
		if err != nil {
			return 0, err
		}
		if op == wsconn.OpBinary {
			w.buf = msg
		}
	}

	n := copy(p, w.buf)
	w.buf = w.buf[n:]

	return n, nil
}

// nativeRFBHandler serves a native worker's screen to the viewer's RFB client.
func (s *Server) nativeRFBHandler(w http.ResponseWriter, r *http.Request, rawID string) {
	sess, err := s.Desktops.NativeSession(rawID)
	switch {
	case errors.Is(err, desktop.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "desktop is not running", http.StatusServiceUnavailable)
		return
	}

	access, err := s.Desktops.ScreenAccess(r.Context(), rawID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	c, err := wsconn.Upgrade(w, r)
	if err != nil {
		return
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.markDesktopViewed(ctx, access.ID)

	conn := &rfbConn{sess: sess, c: c, in: &wsBytes{c: c}, name: access.Name, record: s.recordWorkerInput(access.ID, sess)}
	if err := conn.serve(ctx, cancel, access.Password); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		log.Printf("screen of %s: %v", access.ID, err)
	}
}

// rfbConn is one viewer's RFB session on a worker's desktop.
type rfbConn struct {
	sess *winhost.Session
	c    *wsconn.Conn
	in   io.Reader
	name string
	// record is given each input before it is applied, for a demonstration
	// being recorded (recordWorkerInput).
	record func(hostGUIRequest)

	mu sync.Mutex
	// wantsCursor and wantsSize are what the client said it understands.
	wantsCursor, wantsSize bool
	// asked is a FramebufferUpdateRequest not yet answered: an update is only
	// ever sent in answer to one.
	asked bool
	// queue is the pictures waiting for a request.
	queue []winhost.Update
	wake  chan struct{}

	// The keyboard and mouse as the client last reported them.
	buttons          byte
	ctrl, shift, alt bool
	// clipboard is what the viewer last put on the "remote" clipboard: what
	// Ctrl+V types into the app, since the app's own clipboard is this
	// computer's and is not the viewer's to overwrite.
	clipboard string
}

func (c *rfbConn) send(b []byte) error { return c.c.Write(wsconn.OpBinary, b) }

// serve runs the session until the viewer leaves or the worker's desktop ends.
func (c *rfbConn) serve(ctx context.Context, cancel context.CancelFunc, password string) error {
	if err := c.handshake(password); err != nil {
		return err
	}

	v := c.sess.Watch()
	defer v.Close()

	// The screen's size is the first whole picture's.
	var first winhost.Update
	select {
	case u, ok := <-v.C:
		if !ok {
			return errors.New("the worker's desktop ended")
		}
		first = u
	case <-time.After(rfbFirstPicture):
		return errors.New("the worker's desktop sent no picture")
	case <-ctx.Done():
		return ctx.Err()
	}

	init := make([]byte, 0, 24+len(c.name))
	init = binary.BigEndian.AppendUint16(init, uint16(first.W))
	init = binary.BigEndian.AppendUint16(init, uint16(first.H))
	// 32 bits a pixel, 24 of colour, little-endian, true colour, 8 bits each
	// of red, green and blue. Pictures go as JPEG, which this does not touch.
	init = append(init, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0)
	init = binary.BigEndian.AppendUint32(init, uint32(len(c.name)))
	init = append(init, c.name...)
	if err := c.send(init); err != nil {
		return err
	}

	c.wake = make(chan struct{}, 1)
	c.queue = []winhost.Update{first}
	width, height := first.W, first.H

	go func() {
		defer cancel()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		cursorSent := false

		for {
			select {
			case u, ok := <-v.C:
				if !ok {
					return
				}
				c.mu.Lock()
				if u.Whole {
					c.queue = c.queue[:0] // everything before a whole picture is under it
				}
				c.queue = append(c.queue, u)
				c.mu.Unlock()
			case <-c.wake:
			case <-c.sess.Done():
				return
			case <-ping.C:
				if c.c.Write(wsconn.OpPing, nil) != nil {
					return
				}
				continue
			case <-ctx.Done():
				return
			}

			c.mu.Lock()
			if !c.asked || len(c.queue) == 0 {
				c.mu.Unlock()
				continue
			}
			pictures := c.queue
			c.queue = nil
			c.asked = false
			cursor, sizes := c.wantsCursor && !cursorSent, c.wantsSize
			c.mu.Unlock()

			var rects [][]byte
			if cursor {
				rects = append(rects, rfbArrowCursor())
				cursorSent = true
			}
			for _, u := range pictures {
				if u.Whole && (u.W != width || u.H != height) && sizes {
					width, height = u.W, u.H
					rects = append(rects, rfbRect(0, 0, u.W, u.H, rfbEncodingDesktopSize, nil))
				}
				if u.X+u.Rw > width || u.Y+u.Rh > height {
					continue // a picture of a size this client was never told of
				}
				rects = append(rects, rfbRect(u.X, u.Y, u.Rw, u.Rh, rfbEncodingTight, rfbTightJPEG(u.JPEG)))
			}
			if len(rects) == 0 {
				c.mu.Lock()
				c.asked = true
				c.mu.Unlock()
				continue
			}

			update := []byte{0, 0, byte(len(rects) >> 8), byte(len(rects))}
			for _, rect := range rects {
				update = append(update, rect...)
			}
			if c.send(update) != nil {
				return
			}
		}
	}()

	inputs := newRFBInputs()
	go c.apply(ctx, inputs)
	defer inputs.close()

	return c.read(inputs)
}

// handshake is RFB 3.8's opening: versions, VNC authentication, ClientInit.
func (c *rfbConn) handshake(password string) error {
	if err := c.send([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	var version [12]byte
	if _, err := io.ReadFull(c.in, version[:]); err != nil {
		return err
	}

	if err := c.send([]byte{1, 2}); err != nil { // one security type: VNC authentication
		return err
	}
	var chosen [1]byte
	if _, err := io.ReadFull(c.in, chosen[:]); err != nil {
		return err
	}

	challenge := make([]byte, 16)
	if _, err := rand.Read(challenge); err != nil {
		return err
	}
	if err := c.send(challenge); err != nil {
		return err
	}
	answer := make([]byte, 16)
	if _, err := io.ReadFull(c.in, answer); err != nil {
		return err
	}

	want, err := desktop.VNCAuthResponse(challenge, password)
	if chosen[0] != 2 || password == "" || err != nil || !bytes.Equal(answer, want) {
		reason := "the password was refused"
		refusal := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 1}, uint32(len(reason)))
		c.send(append(refusal, reason...))
		return errors.New("a viewer's password was refused")
	}
	if err := c.send([]byte{0, 0, 0, 0}); err != nil {
		return err
	}

	var shared [1]byte // ClientInit: every viewer shares the screen
	_, err = io.ReadFull(c.in, shared[:])

	return err
}

// read takes the client's messages until it leaves.
func (c *rfbConn) read(inputs *rfbInputs) error {
	var b [20]byte

	for {
		if _, err := io.ReadFull(c.in, b[:1]); err != nil {
			return err
		}

		switch b[0] {
		case 0: // SetPixelFormat: pictures are JPEG whatever it asks for
			if _, err := io.ReadFull(c.in, b[:19]); err != nil {
				return err
			}

		case 2: // SetEncodings
			if _, err := io.ReadFull(c.in, b[:3]); err != nil {
				return err
			}
			list := make([]byte, 4*int(binary.BigEndian.Uint16(b[1:3])))
			if _, err := io.ReadFull(c.in, list); err != nil {
				return err
			}
			c.mu.Lock()
			for i := 0; i+4 <= len(list); i += 4 {
				switch int32(binary.BigEndian.Uint32(list[i:])) {
				case rfbEncodingCursor:
					c.wantsCursor = true
				case rfbEncodingDesktopSize:
					c.wantsSize = true
				}
			}
			c.mu.Unlock()

		case 3: // FramebufferUpdateRequest
			if _, err := io.ReadFull(c.in, b[:9]); err != nil {
				return err
			}
			if b[0] == 0 {
				go c.sess.Do("keyframe") // not incremental: the whole picture again
			}
			c.mu.Lock()
			c.asked = true
			c.mu.Unlock()
			select {
			case c.wake <- struct{}{}:
			default:
			}

		case 4: // KeyEvent
			if _, err := io.ReadFull(c.in, b[:7]); err != nil {
				return err
			}
			inputs.push(rfbInput{kind: rfbKey, down: b[0] != 0, keysym: binary.BigEndian.Uint32(b[3:7])})

		case 5: // PointerEvent
			if _, err := io.ReadFull(c.in, b[:5]); err != nil {
				return err
			}
			inputs.push(rfbInput{kind: rfbPointer, buttons: b[0], x: int(binary.BigEndian.Uint16(b[1:3])), y: int(binary.BigEndian.Uint16(b[3:5]))})

		case 6: // ClientCutText
			if _, err := io.ReadFull(c.in, b[:7]); err != nil {
				return err
			}
			length := int32(binary.BigEndian.Uint32(b[3:7]))
			if length < 0 || length > 1<<20 {
				return errors.New("the viewer sent a clipboard this does not read")
			}
			text := make([]byte, length)
			if _, err := io.ReadFull(c.in, text); err != nil {
				return err
			}
			inputs.push(rfbInput{kind: rfbCutText, text: latin1(text)})

		default:
			return errors.New("the viewer sent a message this does not know")
		}
	}
}

// latin1 reads RFB's clipboard text, which is ISO 8859-1.
func latin1(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}

	return string(runes)
}

// toLatin1 is text as RFB's clipboard carries it; what it cannot is a "?".
func toLatin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			r = '?'
		}
		out = append(out, byte(r))
	}

	return out
}

// rfbRect is one rectangle of a FramebufferUpdate.
func rfbRect(x, y, w, h int, encoding int32, data []byte) []byte {
	rect := make([]byte, 12, 12+len(data))
	binary.BigEndian.PutUint16(rect[0:], uint16(x))
	binary.BigEndian.PutUint16(rect[2:], uint16(y))
	binary.BigEndian.PutUint16(rect[4:], uint16(w))
	binary.BigEndian.PutUint16(rect[6:], uint16(h))
	binary.BigEndian.PutUint32(rect[8:], uint32(encoding))

	return append(rect, data...)
}

// rfbTightJPEG wraps a JPEG as the Tight encoding's JPEG rectangle: the
// compression-control byte, then the length in Tight's compact form.
func rfbTightJPEG(jpeg []byte) []byte {
	out := make([]byte, 0, len(jpeg)+4)
	out = append(out, 0x90)

	n := len(jpeg)
	switch {
	case n < 1<<7:
		out = append(out, byte(n))
	case n < 1<<14:
		out = append(out, byte(n&0x7f)|0x80, byte(n>>7))
	default:
		out = append(out, byte(n&0x7f)|0x80, byte(n>>7&0x7f)|0x80, byte(n>>14))
	}

	return append(out, jpeg...)
}

// arrow is the pointer drawn over the desktop: X its outline, o its inside.
// The helper's pictures have no pointer in them, and without one the viewer
// shows none at all.
var arrow = []string{
	"X           ",
	"XX          ",
	"XoX         ",
	"XooX        ",
	"XoooX       ",
	"XooooX      ",
	"XoooooX     ",
	"XooooooX    ",
	"XoooooooX   ",
	"XooooooooX  ",
	"XoooooooooX ",
	"XooooooXXXXX",
	"XoooXooX    ",
	"XooX XooX   ",
	"XoX  XooX   ",
	"XX    XooX  ",
	"X     XooX  ",
	"       XX   ",
}

// rfbArrowCursor is the Cursor pseudo-encoding's rectangle for the arrow:
// its pixels in the client's format, then a bitmask of which are drawn.
func rfbArrowCursor() []byte {
	w, h := len(arrow[0]), len(arrow)
	stride := (w + 7) / 8
	pixels := make([]byte, w*h*4)
	mask := make([]byte, stride*h)

	for y, row := range arrow {
		for x, ch := range row {
			if ch == ' ' {
				continue
			}
			mask[y*stride+x/8] |= 0x80 >> (x % 8)
			if ch == 'o' {
				copy(pixels[(y*w+x)*4:], []byte{255, 255, 255, 0})
			}
		}
	}

	return rfbRect(0, 0, w, h, rfbEncodingCursor, append(pixels, mask...))
}
