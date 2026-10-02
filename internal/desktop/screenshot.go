package desktop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// vncPort is the desktop's raw RFB port - the one Xvnc itself listens on
// (see agent/start_agent_terminal.sh's stack: `Xvnc :1 ... -rfbport 5901`),
// not the 6901 websockify serves noVNC's assets and WebSocket on.
//
// Thumbnails talk RFB here directly rather than going through 6901, because
// websockify's port is the *browser's* path to a desktop: reusing it would
// mean speaking WebSocket to reach a protocol the control plane can read
// straight off a TCP socket.
const vncPort = "5901"

// Screenshot capture tuning. captureTimeout bounds one whole capture
// (connect, RFB handshake, and a full framebuffer read); a desktop that is
// mid-boot or wedged must never hold a dashboard request open.
const (
	captureTimeout = 3 * time.Second

	// screenshotTTL is how long one captured frame is reused. Every browser
	// showing the dashboard asks for every desktop's thumbnail, so without
	// this a second viewer (or a background tab that never stopped
	// refreshing) would double the capture load on every desktop.
	screenshotTTL = 3 * time.Second

	// jpegQuality is deliberately modest: these are thumbnails a few hundred
	// pixels wide, and the smaller the frame the sooner the dashboard's
	// connection to the control plane is free again.
	jpegQuality = 70
)

// RFB client-to-server message types and the single encoding this client
// understands. Only Raw is offered, so the server cannot answer with a
// compression scheme there is no decoder here for.
const (
	msgSetPixelFormat           = 0
	msgSetEncodings             = 2
	msgFramebufferUpdateRequest = 3

	msgFramebufferUpdate   = 0
	msgSetColourMapEntries = 1
	msgBell                = 2
	msgServerCutText       = 3

	encodingRaw = 0
)

// shotEntry is one desktop's cached thumbnail. Its mutex is held for the
// duration of a capture, so concurrent dashboard requests for the same
// desktop wait for the frame already being fetched instead of each opening
// their own VNC session.
type shotEntry struct {
	mu      sync.Mutex
	data    []byte
	at      time.Time
	lastErr error
	errAt   time.Time
}

// shotCacheFor returns the cache entry for a desktop, creating it on first
// use. It only guards the map itself - the returned entry carries its own
// lock, so a slow capture of one desktop never blocks another.
func (m *Manager) shotCacheFor(desktopID string) *shotEntry {
	m.shotMu.Lock()
	defer m.shotMu.Unlock()

	if m.shots == nil {
		m.shots = make(map[string]*shotEntry)
	}

	existing, ok := m.shots[desktopID]
	if ok {
		return existing
	}

	created := &shotEntry{}
	m.shots[desktopID] = created

	return created
}

// shotFilePath returns the filesystem path where the latest thumbnail for
// sanitizedID is persisted.
func shotFilePath(sanitizedID string) string {
	return filepath.Join(sqlitedb.DataDir(), "shots", sanitizedID+".jpg")
}

// saveShotToDisk atomically writes the JPEG thumbnail data to disk.
func saveShotToDisk(sanitizedID string, data []byte) {
	if len(data) == 0 {
		return
	}
	dir := filepath.Join(sqlitedb.DataDir(), "shots")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	tmp := filepath.Join(dir, sanitizedID+".tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, sanitizedID+".jpg"))
}

// loadShotFromDisk loads the persisted JPEG thumbnail data for sanitizedID from disk.
func loadShotFromDisk(sanitizedID string) ([]byte, error) {
	return os.ReadFile(shotFilePath(sanitizedID))
}

// Screenshot returns a JPEG thumbnail of a desktop's current screen, scaled
// so its longest edge is at most maxWidth pixels wide.
//
// It reads the framebuffer straight off the desktop's Xvnc over the Decurix
// bridge network, which is what lets the dashboard show what a desktop is
// actually doing without embedding a noVNC client per card. Frames are cached
// for screenshotTTL and captured one-at-a-time per desktop.
func (m *Manager) Screenshot(ctx context.Context, rawID string, maxWidth int) ([]byte, error) {
	sanitizedID := sanitizeName(rawID)

	entry := m.shotCacheFor(sanitizedID)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.data != nil && time.Since(entry.at) < screenshotTTL {
		m.touchViewed(sanitizedID)
		return entry.data, nil
	}

	// Negative caching: if capture failed very recently (e.g. desktop booting or VNC down),
	// return the last error immediately instead of re-dialing and stalling dashboard threads.
	if entry.lastErr != nil && time.Since(entry.errAt) < 2*time.Second {
		return nil, entry.lastErr
	}

	if m.native != nil {
		encoded, err := m.native.screenshot(ctx, sanitizedID, maxWidth)
		if err != nil {
			entry.lastErr = err
			entry.errAt = time.Now()
			return nil, err
		}
		m.touchViewed(sanitizedID)
		entry.data, entry.at, entry.lastErr = encoded, time.Now(), nil
		return encoded, nil
	}

	// Only a cache miss pays for the ContainerInspect captureTarget needs to
	// resolve the desktop's address and password - a cache hit above never
	// touches Docker at all.
	addr, password, _, err := m.captureTarget(ctx, rawID)
	if err != nil {
		if errors.Is(err, ErrPaused) {
			if entry.data != nil {
				// Container is frozen: its screen is completely static. Return
				// the frozen thumbnail without attempting to dial VNC or failing.
				m.touchViewed(sanitizedID)
				return entry.data, nil
			}
			// Fall back to the screenshot persisted on disk before the pause or restart.
			if diskData, diskErr := loadShotFromDisk(sanitizedID); diskErr == nil && len(diskData) > 0 {
				entry.data = diskData
				entry.at = time.Now()
				entry.lastErr = nil
				m.touchViewed(sanitizedID)
				return diskData, nil
			}
		}
		entry.lastErr = err
		entry.errAt = time.Now()
		return nil, err
	}

	m.touchViewed(sanitizedID)

	frame, err := captureFrame(ctx, addr, password)
	if err != nil {
		entry.lastErr = err
		entry.errAt = time.Now()
		return nil, err
	}

	encoded, err := encodeThumbnail(frame, maxWidth)
	if err != nil {
		entry.lastErr = err
		entry.errAt = time.Now()
		return nil, err
	}

	entry.data = encoded
	entry.at = time.Now()
	entry.lastErr = nil
	go saveShotToDisk(sanitizedID, encoded)

	return encoded, nil
}

// captureTarget resolves a running desktop's raw VNC address and the password
// its Xvnc was started with. Both are read from the container itself rather
// than from the control plane's own DEFAULT_PASSWORD, so a desktop created
// before that value changed still has its screen readable.
func (m *Manager) captureTarget(ctx context.Context, rawID string) (addr, password, sanitizedID string, err error) {
	sanitizedID = sanitizeName(rawID)
	name := "desktop_" + sanitizedID

	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil {
		return "", "", sanitizedID, ErrNotFound
	}

	if inspect.State == nil || !inspect.State.Running {
		return "", "", sanitizedID, ErrNotRunning
	}

	if inspect.State.Paused {
		return "", "", sanitizedID, ErrPaused
	}

	networkInfo, ok := inspect.NetworkSettings.Networks[NetworkName]
	if !ok || networkInfo == nil {
		return "", "", sanitizedID, ErrNoNetwork
	}

	if networkInfo.IPAddress == "" {
		return "", "", sanitizedID, ErrNoAddress
	}

	if inspect.Config != nil {
		for _, entry := range inspect.Config.Env {
			if value, found := strings.CutPrefix(entry, "VNC_PW="); found {
				password = value
				break
			}
		}
	}

	return net.JoinHostPort(networkInfo.IPAddress, vncPort), password, sanitizedID, nil
}

// captureFrame opens one RFB session against a desktop's Xvnc and returns its
// full screen as an image. The session is deliberately short-lived: connect,
// authenticate, ask for one non-incremental update, read it, hang up.
func captureFrame(ctx context.Context, addr, password string) (*image.RGBA, error) {
	dialer := net.Dialer{
		Timeout: 2 * time.Second,
	}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to desktop screen: %w", err)
	}

	defer conn.Close()

	deadline := time.Now().Add(captureTimeout)

	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	reader := bufio.NewReaderSize(conn, 64*1024)

	if err := rfbHandshake(reader, conn, password); err != nil {
		return nil, err
	}

	width, height, err := rfbInit(reader, conn)
	if err != nil {
		return nil, err
	}

	return rfbReadFrame(reader, conn, width, height)
}

// rfbHandshake performs the RFB 3.8 version and security exchange, answering
// a VNC-Authentication challenge with the desktop's own password.
func rfbHandshake(reader io.Reader, writer io.Writer, password string) error {
	version := make([]byte, 12)

	if _, err := io.ReadFull(reader, version); err != nil {
		return fmt.Errorf("read RFB version: %w", err)
	}

	if !bytes.HasPrefix(version, []byte("RFB ")) {
		return fmt.Errorf("not an RFB server")
	}

	// Answer 3.8 regardless of the server's minor version: TigerVNC speaks
	// it, and it is the version whose SecurityResult carries a reason string
	// on failure, which is the difference between a readable error and none.
	if _, err := writer.Write([]byte("RFB 003.008\n")); err != nil {
		return fmt.Errorf("write RFB version: %w", err)
	}

	count := make([]byte, 1)

	if _, err := io.ReadFull(reader, count); err != nil {
		return fmt.Errorf("read security types: %w", err)
	}

	if count[0] == 0 {
		return fmt.Errorf("desktop screen refused the connection: %s", rfbReadReason(reader))
	}

	types := make([]byte, count[0])

	if _, err := io.ReadFull(reader, types); err != nil {
		return fmt.Errorf("read security types: %w", err)
	}

	const (
		securityNone    = 1
		securityVNCAuth = 2
	)

	chosen := byte(0)

	for _, candidate := range types {
		if candidate == securityVNCAuth || (candidate == securityNone && chosen == 0) {
			chosen = candidate
		}
	}

	if chosen == 0 {
		return fmt.Errorf("desktop screen offers no supported authentication")
	}

	if _, err := writer.Write([]byte{chosen}); err != nil {
		return err
	}

	if chosen == securityVNCAuth {
		challenge := make([]byte, 16)

		if _, err := io.ReadFull(reader, challenge); err != nil {
			return fmt.Errorf("read auth challenge: %w", err)
		}

		response, err := vncAuthResponse(challenge, password)
		if err != nil {
			return err
		}

		if _, err := writer.Write(response); err != nil {
			return err
		}
	}

	result := make([]byte, 4)

	if _, err := io.ReadFull(reader, result); err != nil {
		return fmt.Errorf("read auth result: %w", err)
	}

	if binary.BigEndian.Uint32(result) != 0 {
		return fmt.Errorf("desktop screen rejected the password: %s", rfbReadReason(reader))
	}

	return nil
}

// rfbReadReason reads the length-prefixed failure string RFB 3.8 sends after
// a rejection. A malformed or absent reason is reported as unknown rather
// than turning one failure into two.
func rfbReadReason(reader io.Reader) string {
	length := make([]byte, 4)

	if _, err := io.ReadFull(reader, length); err != nil {
		return "unknown reason"
	}

	size := binary.BigEndian.Uint32(length)

	if size == 0 || size > 4096 {
		return "unknown reason"
	}

	reason := make([]byte, size)

	if _, err := io.ReadFull(reader, reason); err != nil {
		return "unknown reason"
	}

	return string(reason)
}

// vncAuthResponse computes the VNC-Authentication answer: the 16-byte
// challenge DES-encrypted with the password as the key. VNC reverses the bit
// order of every key byte, a quirk of the original implementation that every
// server still expects.
func vncAuthResponse(challenge []byte, password string) ([]byte, error) {
	key := make([]byte, 8)

	copy(key, password)

	for i, b := range key {
		key[i] = bits8Reverse(b)
	}

	block, err := des.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build auth cipher: %w", err)
	}

	response := make([]byte, len(challenge))

	for offset := 0; offset+8 <= len(challenge); offset += 8 {
		block.Encrypt(response[offset:offset+8], challenge[offset:offset+8])
	}

	return response, nil
}

// VNCAuthResponse is the answer VNC authentication expects to a challenge for
// a password: what a native worker's own VNC endpoint checks a viewer against.
func VNCAuthResponse(challenge []byte, password string) ([]byte, error) {
	return vncAuthResponse(challenge, password)
}

// bits8Reverse reverses the bit order of a single byte.
func bits8Reverse(b byte) byte {
	var out byte

	for i := 0; i < 8; i++ {
		out <<= 1
		out |= (b >> i) & 1
	}

	return out
}

// rfbInit completes ClientInit/ServerInit and pins the session to a pixel
// format this client can decode, returning the desktop's screen size.
func rfbInit(reader io.Reader, writer io.Writer) (width, height int, err error) {
	// shared=1: never disconnect whoever is already looking at this desktop.
	// A thumbnail must be invisible to the human using the screen.
	if _, err := writer.Write([]byte{1}); err != nil {
		return 0, 0, err
	}

	header := make([]byte, 24)

	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, 0, fmt.Errorf("read desktop size: %w", err)
	}

	width = int(binary.BigEndian.Uint16(header[0:2]))
	height = int(binary.BigEndian.Uint16(header[2:4]))

	nameLength := binary.BigEndian.Uint32(header[20:24])

	if nameLength > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(nameLength)); err != nil {
			return 0, 0, fmt.Errorf("read desktop name: %w", err)
		}
	}

	if width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, fmt.Errorf("desktop reported an unusable screen size %dx%d", width, height)
	}

	// Ask for little-endian 32-bit true colour so every pixel is a fixed
	// 4 bytes in BGRA order - the server converts, and there is exactly one
	// layout to decode below.
	setPixelFormat := []byte{
		msgSetPixelFormat, 0, 0, 0,
		32,     // bits per pixel
		24,     // depth
		0,      // big-endian flag
		1,      // true-colour flag
		0, 255, // red max
		0, 255, // green max
		0, 255, // blue max
		16, // red shift
		8,  // green shift
		0,  // blue shift
		0, 0, 0,
	}

	if _, err := writer.Write(setPixelFormat); err != nil {
		return 0, 0, err
	}

	// Offer Raw only. Anything else would need a decoder here, and a
	// thumbnail is not worth carrying one.
	setEncodings := make([]byte, 8)
	setEncodings[0] = msgSetEncodings
	binary.BigEndian.PutUint16(setEncodings[2:4], 1)
	binary.BigEndian.PutUint32(setEncodings[4:8], uint32(encodingRaw))

	if _, err := writer.Write(setEncodings); err != nil {
		return 0, 0, err
	}

	return width, height, nil
}

// rfbReadFrame requests one full (non-incremental) framebuffer update and
// paints every rectangle it returns into a single image.
func rfbReadFrame(reader io.Reader, writer io.Writer, width, height int) (*image.RGBA, error) {
	request := make([]byte, 10)
	request[0] = msgFramebufferUpdateRequest
	request[1] = 0 // non-incremental: send the whole screen, not a delta
	binary.BigEndian.PutUint16(request[6:8], uint16(width))
	binary.BigEndian.PutUint16(request[8:10], uint16(height))

	if _, err := writer.Write(request); err != nil {
		return nil, err
	}

	frame := image.NewRGBA(image.Rect(0, 0, width, height))

	for {
		messageType := make([]byte, 1)

		if _, err := io.ReadFull(reader, messageType); err != nil {
			return nil, fmt.Errorf("read screen update: %w", err)
		}

		switch messageType[0] {
		case msgFramebufferUpdate:
			if err := rfbPaintUpdate(reader, frame); err != nil {
				return nil, err
			}

			return frame, nil

		// The server may volunteer these before the update lands; skipping
		// them keeps the read in sync rather than treating a bell as data.
		case msgSetColourMapEntries:
			if err := rfbSkipColourMap(reader); err != nil {
				return nil, err
			}

		case msgBell:

		case msgServerCutText:
			if err := rfbSkipCutText(reader); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("unexpected RFB message type %d", messageType[0])
		}
	}
}

// rfbPaintUpdate reads one FramebufferUpdate's rectangles into frame.
func rfbPaintUpdate(reader io.Reader, frame *image.RGBA) error {
	var header [3]byte

	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return fmt.Errorf("read update header: %w", err)
	}

	bounds := frame.Bounds()
	rectangles := binary.BigEndian.Uint16(header[1:3])

	// Reuse a single row buffer across all rectangles and scanlines.
	rowBuf := make([]byte, bounds.Dx()*4)

	for i := 0; i < int(rectangles); i++ {
		var spec [12]byte

		if _, err := io.ReadFull(reader, spec[:]); err != nil {
			return fmt.Errorf("read rectangle header: %w", err)
		}

		x := int(binary.BigEndian.Uint16(spec[0:2]))
		y := int(binary.BigEndian.Uint16(spec[2:4]))
		w := int(binary.BigEndian.Uint16(spec[4:6]))
		h := int(binary.BigEndian.Uint16(spec[6:8]))
		encoding := int32(binary.BigEndian.Uint32(spec[8:12]))

		if encoding != encodingRaw {
			return fmt.Errorf("desktop sent unsupported encoding %d", encoding)
		}

		if w == 0 || h == 0 {
			continue
		}

		if x < 0 || y < 0 || x+w > bounds.Dx() || y+h > bounds.Dy() {
			return fmt.Errorf("desktop sent a rectangle outside the screen")
		}

		row := rowBuf[:w*4]

		for line := 0; line < h; line++ {
			if _, err := io.ReadFull(reader, row); err != nil {
				return fmt.Errorf("read rectangle pixels: %w", err)
			}

			offset := frame.PixOffset(x, y+line)

			// Little-endian BGRA on the wire (see the pixel format pinned in
			// rfbInit) to RGBA in the image, alpha forced opaque.
			for px := 0; px < w; px++ {
				frame.Pix[offset+0] = row[px*4+2]
				frame.Pix[offset+1] = row[px*4+1]
				frame.Pix[offset+2] = row[px*4+0]
				frame.Pix[offset+3] = 255

				offset += 4
			}
		}
	}

	return nil
}

// rfbSkipColourMap discards a SetColourMapEntries message.
func rfbSkipColourMap(reader io.Reader) error {
	header := make([]byte, 5)

	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}

	colours := binary.BigEndian.Uint16(header[3:5])

	_, err := io.CopyN(io.Discard, reader, int64(colours)*6)

	return err
}

// rfbSkipCutText discards a ServerCutText message.
func rfbSkipCutText(reader io.Reader) error {
	header := make([]byte, 7)

	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}

	_, err := io.CopyN(io.Discard, reader, int64(binary.BigEndian.Uint32(header[3:7])))

	return err
}

var jpegBufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// encodeThumbnail scales a captured screen down so its width is at most
// maxWidth and encodes it as JPEG.
func encodeThumbnail(frame *image.RGBA, maxWidth int) ([]byte, error) {
	scaled := downscale(frame, maxWidth)

	buf := jpegBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer jpegBufPool.Put(buf)

	if err := jpeg.Encode(buf, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("encode screen: %w", err)
	}

	res := make([]byte, buf.Len())
	copy(res, buf.Bytes())
	return res, nil
}

// downscale box-filters an image to at most maxWidth wide, preserving aspect
// ratio. Averaging whole source boxes (rather than sampling one pixel per
// destination) is what keeps a 1280-wide desktop's text from turning into
// aliased noise at thumbnail size; it is cheap enough that the capture
// itself, not this, dominates.
func downscale(src *image.RGBA, maxWidth int) image.Image {
	bounds := src.Bounds()

	if maxWidth <= 0 || bounds.Dx() <= maxWidth {
		return src
	}

	dstWidth := maxWidth
	dstHeight := bounds.Dy() * maxWidth / bounds.Dx()

	if dstHeight < 1 {
		dstHeight = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstWidth, dstHeight))

	for dy := 0; dy < dstHeight; dy++ {
		y0 := dy * bounds.Dy() / dstHeight
		y1 := (dy + 1) * bounds.Dy() / dstHeight

		if y1 <= y0 {
			y1 = y0 + 1
		}

		for dx := 0; dx < dstWidth; dx++ {
			x0 := dx * bounds.Dx() / dstWidth
			x1 := (dx + 1) * bounds.Dx() / dstWidth

			if x1 <= x0 {
				x1 = x0 + 1
			}

			var r, g, b, count uint32

			for y := y0; y < y1; y++ {
				offset := src.PixOffset(x0, y)

				for x := x0; x < x1; x++ {
					r += uint32(src.Pix[offset+0])
					g += uint32(src.Pix[offset+1])
					b += uint32(src.Pix[offset+2])
					count++

					offset += 4
				}
			}

			if count == 0 {
				count = 1
			}

			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(r / count),
				G: uint8(g / count),
				B: uint8(b / count),
				A: 255,
			})
		}
	}

	return dst
}
