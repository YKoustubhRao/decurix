package desktop

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"sync"
	"time"
)

// A watched desktop's thumbnails come from one RFB session kept open while someone looks (Phase 2 C), not a new
// session per capture: each capture used to cost a TCP connect, the handshake and the whole screen raw (1280x720x4,
// 3.6 MB) every sweep, changed or not. The session asks Xvnc for incremental updates only, and Xvnc answers one
// when something on the screen changed, so a screen that does not change costs no bytes and no JPEG encode, and a
// changed one only its changed rectangles. A session no thumbnail asked of for sessionIdle is closed; the next
// capture opens another.
var (
	// sessionIdle is how long a session no viewer asked of stays open.
	sessionIdle = 30 * time.Second
	// sessionIdleCheck is how often a session checks that.
	sessionIdleCheck = 5 * time.Second
	// sessionGap is the least time between two update requests: a screen that never stops changing (a video, a
	// spinner) costs at most one update per gap, not Xvnc's every frame.
	sessionGap = 2 * time.Second
)

// The encodings a session offers: Raw, CopyRect (a moved region is two coordinates, not its pixels) and the
// DesktopSize pseudo-encoding (the screen was resized: a new framebuffer, sent whole). Nothing that needs a decoder
// this file does not carry.
const (
	encodingCopyRect    = 1
	encodingDesktopSize = -223
)

// errSessionIdle, errSessionDropped: why a session ended without the screen failing.
var (
	errSessionIdle    = errors.New("no viewer asked for the screen")
	errSessionDropped = errors.New("the desktop's lifecycle changed")
)

// shotSession is one open RFB session against a desktop's Xvnc and the screen it has painted so far.
type shotSession struct {
	conn   net.Conn
	reader *bufio.Reader
	target *shotTarget
	name   string // the screen's RFB desktop name

	mu       sync.Mutex
	frame    *image.RGBA
	version  uint64    // bumped by every update painted into frame
	wanted   time.Time // when a thumbnail last asked
	needFull bool      // the next request asks for the whole screen (after a resize)
	// blank is set from a resize until the whole screen has been painted again at its new size: the frame is empty
	// meanwhile, and a thumbnail taken of it would be black (a window being fitted to a desktop's screen resizes it
	// again and again).
	blank bool

	done      chan struct{}
	closeOnce sync.Once
	err       error // why it ended; read after done
}

// openShotSession connects to a desktop's screen, authenticates, and paints its whole screen once before it returns
// (within captureTimeout, as one capture was); the session then keeps the screen up to date on its own.
func openShotSession(ctx context.Context, target *shotTarget) (*shotSession, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second,
		// A screen that went away without a word (a container killed) is noticed in about 25 s, not Linux's hours.
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3}}
	conn, err := dialer.DialContext(ctx, "tcp", target.addr)
	if err != nil {
		return nil, fmt.Errorf("connect to desktop screen: %w", err)
	}
	deadline := time.Now().Add(captureTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	s := &shotSession{conn: conn, reader: bufio.NewReaderSize(conn, 64*1024), target: target, wanted: time.Now(),
		done: make(chan struct{})}
	fail := func(err error) (*shotSession, error) {
		conn.Close()
		return nil, err
	}
	if err := rfbHandshake(s.reader, conn, target.password); err != nil {
		return fail(err)
	}
	width, height, name, err := rfbInit(s.reader, conn, encodingRaw, encodingCopyRect, encodingDesktopSize)
	if err != nil {
		return fail(err)
	}
	s.name = name
	s.frame = image.NewRGBA(image.Rect(0, 0, width, height))
	if err := s.request(false); err != nil {
		return fail(err)
	}
	if err := s.readUpdate(); err != nil {
		return fail(err)
	}
	// From here the session waits for changes as long as they take: no deadline (keepalive notices a dead peer).
	_ = conn.SetDeadline(time.Time{})
	go s.run()
	go s.watchIdle()
	return s, nil
}

// run asks for the next incremental update at most once per sessionGap and paints each one, until the session ends.
func (s *shotSession) run() {
	last := time.Now()
	for {
		if wait := sessionGap - time.Since(last); wait > 0 {
			select {
			case <-time.After(wait):
			case <-s.done:
				return
			}
		}
		s.mu.Lock()
		full := s.needFull
		s.needFull = false
		s.mu.Unlock()
		last = time.Now()
		if err := s.request(!full); err != nil {
			s.close(err)
			return
		}
		if err := s.readUpdate(); err != nil {
			s.close(err)
			return
		}
		if full {
			// The answer to a request for the whole screen: it is painted again.
			s.mu.Lock()
			s.blank = false
			s.version++
			s.mu.Unlock()
		}
	}
}

// watchIdle closes the session once no thumbnail asked of it for sessionIdle.
func (s *shotSession) watchIdle() {
	tick := time.NewTicker(sessionIdleCheck)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			s.mu.Lock()
			idle := time.Since(s.wanted) > sessionIdle
			s.mu.Unlock()
			if idle {
				s.close(errSessionIdle)
				return
			}
		}
	}
}

// close ends the session for err; the first reason is kept.
func (s *shotSession) close(err error) {
	s.closeOnce.Do(func() {
		s.err = err
		close(s.done)
		_ = s.conn.Close()
	})
}

// alive reports whether the session is still open.
func (s *shotSession) alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// ended is why the session ended (nil while it is open).
func (s *shotSession) ended() error {
	if s.alive() {
		return nil
	}
	return s.err
}

// snapshot marks the screen wanted now and returns the version painted so far, with a copy of the screen unless
// that version is have (haveOK): a thumbnail already encoded from it needs nothing.
func (s *shotSession) snapshot(have uint64, haveOK bool) (*image.RGBA, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wanted = time.Now()
	if s.blank || (haveOK && have == s.version) {
		// Nothing to encode: the picture a thumbnail already has stands (and while the screen is being repainted after a
		// resize there is no picture to take).
		return nil, s.version
	}
	frame := image.NewRGBA(s.frame.Rect)
	copy(frame.Pix, s.frame.Pix)
	return frame, s.version
}

// request sends a FramebufferUpdateRequest for the whole screen: incremental (only what changed, answered when
// something does) or not.
func (s *shotSession) request(incremental bool) error {
	s.mu.Lock()
	b := s.frame.Bounds()
	s.mu.Unlock()
	req := make([]byte, 10)
	req[0] = msgFramebufferUpdateRequest
	if incremental {
		req[1] = 1
	}
	binary.BigEndian.PutUint16(req[6:8], uint16(b.Dx()))
	binary.BigEndian.PutUint16(req[8:10], uint16(b.Dy()))
	_, err := s.conn.Write(req)
	return err
}

// readUpdate reads server messages until one FramebufferUpdate has been painted, skipping what else the server
// volunteers (a bell, cut text, a colour map).
func (s *shotSession) readUpdate() error {
	for {
		kind, err := s.reader.ReadByte()
		if err != nil {
			return fmt.Errorf("read screen update: %w", err)
		}
		switch kind {
		case msgFramebufferUpdate:
			return s.paint()
		case msgSetColourMapEntries:
			if err := rfbSkipColourMap(s.reader); err != nil {
				return err
			}
		case msgBell:
		case msgServerCutText:
			if err := rfbSkipCutText(s.reader); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected RFB message type %d", kind)
		}
	}
}

// paint reads one FramebufferUpdate's rectangles into the session's screen and bumps its version.
func (s *shotSession) paint() error {
	var header [3]byte
	if _, err := io.ReadFull(s.reader, header[:]); err != nil {
		return fmt.Errorf("read update header: %w", err)
	}
	rects := int(binary.BigEndian.Uint16(header[1:3]))
	var row []byte
	for i := 0; i < rects; i++ {
		var spec [12]byte
		if _, err := io.ReadFull(s.reader, spec[:]); err != nil {
			return fmt.Errorf("read rectangle header: %w", err)
		}
		x := int(binary.BigEndian.Uint16(spec[0:2]))
		y := int(binary.BigEndian.Uint16(spec[2:4]))
		w := int(binary.BigEndian.Uint16(spec[4:6]))
		h := int(binary.BigEndian.Uint16(spec[6:8]))
		encoding := int32(binary.BigEndian.Uint32(spec[8:12]))

		if encoding == encodingDesktopSize {
			if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
				return fmt.Errorf("desktop reported an unusable screen size %dx%d", w, h)
			}
			s.mu.Lock()
			s.frame = image.NewRGBA(image.Rect(0, 0, w, h))
			s.needFull = true
				s.blank = true
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		bounds := s.frame.Bounds()
		s.mu.Unlock()
		if x+w > bounds.Dx() || y+h > bounds.Dy() {
			return fmt.Errorf("desktop sent a rectangle outside the screen")
		}
		switch encoding {
		case encodingRaw:
			if cap(row) < w*4 {
				row = make([]byte, w*4)
			}
			row = row[:w*4]
			for line := 0; line < h; line++ {
				if _, err := io.ReadFull(s.reader, row); err != nil {
					return fmt.Errorf("read rectangle pixels: %w", err)
				}
				s.mu.Lock()
				offset := s.frame.PixOffset(x, y+line)
				pix := s.frame.Pix
				// Little-endian BGRA on the wire (the pixel format rfbInit pins) to RGBA, alpha opaque.
				for px := 0; px < w; px++ {
					pix[offset+0] = row[px*4+2]
					pix[offset+1] = row[px*4+1]
					pix[offset+2] = row[px*4+0]
					pix[offset+3] = 255
					offset += 4
				}
				s.mu.Unlock()
			}
		case encodingCopyRect:
			var src [4]byte
			if _, err := io.ReadFull(s.reader, src[:]); err != nil {
				return fmt.Errorf("read copy source: %w", err)
			}
			sx, sy := int(binary.BigEndian.Uint16(src[0:2])), int(binary.BigEndian.Uint16(src[2:4]))
			if sx+w > bounds.Dx() || sy+h > bounds.Dy() {
				return fmt.Errorf("desktop copied from outside the screen")
			}
			s.mu.Lock()
			copyRect(s.frame, sx, sy, x, y, w, h)
			s.mu.Unlock()
		default:
			return fmt.Errorf("desktop sent unsupported encoding %d", encoding)
		}
	}
	s.mu.Lock()
	s.version++
	s.mu.Unlock()
	return nil
}

// copyRect copies the w x h region at (sx, sy) of frame to (dx, dy), overlapping or not.
func copyRect(frame *image.RGBA, sx, sy, dx, dy, w, h int) {
	tmp := make([]byte, w*4*h)
	for line := 0; line < h; line++ {
		o := frame.PixOffset(sx, sy+line)
		copy(tmp[line*w*4:(line+1)*w*4], frame.Pix[o:o+w*4])
	}
	for line := 0; line < h; line++ {
		o := frame.PixOffset(dx, dy+line)
		copy(frame.Pix[o:o+w*4], tmp[line*w*4:(line+1)*w*4])
	}
}
