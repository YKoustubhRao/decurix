package desktop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/des"
	"crypto/sha256"
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
	"sync/atomic"
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
var vncPort = "5901" // a variable only so a test can stand an RFB server in on a free port

// Screenshot capture tuning. captureTimeout bounds one whole capture
// (connect, RFB handshake, and a full framebuffer read); a desktop that is
// mid-boot or wedged must never hold a dashboard request open.
const (
	captureTimeout = 3 * time.Second

	// screenshotTTL is how long one captured frame is reused. Every browser
	// showing the dashboard asks for every desktop's thumbnail, so without
	// this a second viewer (or a background tab that never stopped
	// refreshing) would double the capture load on every desktop. It is the
	// dashboard's sweep (5 s, index.html SHOT_INTERVAL_MS): one viewer still
	// gets a fresh frame each sweep (a sweep takes longer than 5 s from one
	// capture to the next), and every other viewer shares it (resource plan P1 #4).
	screenshotTTL = 5 * time.Second

	// shotTargetTTL is how long a desktop's VNC address and password, read by one container inspect, are reused by
	// its captures. A failed capture and every lifecycle change (InvalidateProxyTarget) drop it sooner.
	shotTargetTTL = 30 * time.Second

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
	mu sync.Mutex
	// byWidth is each width's own latest capture, with its own time: a viewer at another width gets its own size,
	// and no width's frame is kept past screenshotTTL by a capture made for another (review RES-CP-1: one shared time
	// served a frame k sweeps old with k widths in use).
	byWidth map[int]shotFrame
	// data is the latest encoding of any width: what a paused desktop shows.
	data    []byte
	at      time.Time
	lastErr error
	errAt   time.Time
	// target is the desktop's VNC address and password, reused for shotTargetTTL (captureTarget).
	target atomic.Pointer[shotTarget]
	// savedSum is the digest of the thumbnail last written to disk: an unchanged screen is not written again.
	savedSum [32]byte
	// nameless is a desktop whose screen does not name its container (the RFB desktop name lacks its hostname): its
	// cached address cannot be checked, so every capture asks Docker.
	nameless bool
}

// shotFrame is one width's capture and when it was made.
type shotFrame struct {
	data []byte
	at   time.Time
}

// shotTarget is a desktop's VNC address and password as one container inspect read them, the container they were
// read for and its hostname (which the screen's RFB desktop name carries), and until when they are reused.
type shotTarget struct {
	addr, password string
	containerID    string
	hostname       string
	until          time.Time
}

// saveShot has a thumbnail written to disk, in the background (saveShotToDisk): the request never waits on the disk.
// A variable only so a test can stand in a counter.
var saveShot = func(sanitizedID string, data []byte) { go saveShotToDisk(sanitizedID, data) }

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

	if cached := entry.byWidth[maxWidth]; cached.data != nil && time.Since(cached.at) < screenshotTTL {
		m.touchViewed(sanitizedID)
		return cached.data, nil
	}

	// Negative caching: if capture failed very recently (e.g. desktop booting or VNC down),
	// return the last error immediately instead of re-dialing and stalling dashboard threads.
	if entry.lastErr != nil && time.Since(entry.errAt) < 2*time.Second {
		return nil, entry.lastErr
	}

	// Only a cache miss pays for the ContainerInspect captureTarget needs to
	// resolve the desktop's address and password - a cache hit above never
	// touches Docker at all - and that answer is itself reused for
	// shotTargetTTL, so a capture every sweep is not an inspect every sweep.
	var target *shotTarget
	var err error
	cached := false
	if t := entry.target.Load(); t != nil && time.Now().Before(t.until) && !entry.nameless {
		target, cached = t, true
	} else {
		target, err = m.captureTarget(ctx, rawID)
		if err == nil {
			entry.target.Store(target)
		}
	}
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
				entry.byWidth = nil
				entry.at = time.Now()
				entry.lastErr = nil
				m.touchViewed(sanitizedID)
				return diskData, nil
			}
		}
		return entry.fail(err)
	}

	m.touchViewed(sanitizedID)

	frame, screenName, err := captureFrame(ctx, target.addr, target.password)
	if err != nil {
		// The address may be stale (a restart, a pause): the next capture asks Docker again.
		entry.target.Store(nil)
		return entry.fail(err)
	}
	// The screen must be this desktop's. A cached address is used only while the screen it reaches names the container
	// it was read for: an address another container now holds (a restart, a daemon restart) is never shown as this
	// desktop's, even where desktops share one password (the old default). A screen that does not name its container
	// at all is checked by Docker on every capture instead.
	if !strings.Contains(screenName, target.hostname) {
		entry.target.Store(nil)
		if cached {
			return entry.fail(errors.New("the cached screen address reached another screen"))
		}
		entry.nameless = true
	}

	encoded, err := encodeThumbnail(frame, maxWidth)
	if err != nil {
		return entry.fail(err)
	}

	now := time.Now()
	if entry.byWidth == nil {
		entry.byWidth = map[int]shotFrame{}
	}
	for w, f := range entry.byWidth {
		if now.Sub(f.at) >= screenshotTTL {
			delete(entry.byWidth, w)
		}
	}
	entry.byWidth[maxWidth] = shotFrame{data: encoded, at: now}
	entry.data = encoded
	entry.at = now
	entry.lastErr = nil
	// The disk copy is what a paused or restarted desktop shows; an unchanged screen is not written again.
	if sum := sha256.Sum256(encoded); sum != entry.savedSum {
		entry.savedSum = sum
		saveShot(sanitizedID, encoded)
	}

	return encoded, nil
}

// forgetShotTarget drops a desktop's cached VNC address, so its next capture asks Docker (a lifecycle change).
func (m *Manager) forgetShotTarget(sanitizedID string) {
	m.forgetShotTargetOf(sanitizedID)
}

// forgetShotTargetOf drops the cached VNC address of the desktop a container reference names: its desktop id, its
// container name (desktop_<id>) or its container ID. Every pause and resume calls it, so a frozen desktop's next
// capture asks Docker (and is told it is paused) instead of waiting on a frozen screen.
func (m *Manager) forgetShotTargetOf(ref string) {
	m.shotMu.Lock()
	defer m.shotMu.Unlock()
	id := strings.TrimPrefix(strings.TrimPrefix(ref, "/"), "desktop_")
	for key, entry := range m.shots {
		t := entry.target.Load()
		if key == id || (t != nil && t.containerID != "" && (t.containerID == ref || strings.HasPrefix(t.containerID, ref))) {
			entry.target.Store(nil)
		}
	}
}

// fail remembers a failed capture for the negative cache and returns it. The caller holds e.mu.
func (e *shotEntry) fail(err error) ([]byte, error) {
	e.lastErr = err
	e.errAt = time.Now()
	return nil, err
}

// captureTarget resolves a running desktop's raw VNC address and the password
// its Xvnc was started with. Both are read from the container itself rather
// than from the control plane's own DEFAULT_PASSWORD, so a desktop created
// before that value changed still has its screen readable.
func (m *Manager) captureTarget(ctx context.Context, rawID string) (*shotTarget, error) {
	name := "desktop_" + sanitizeName(rawID)
	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil {
		return nil, ErrNotFound
	}

	if inspect.State == nil || !inspect.State.Running {
		return nil, ErrNotRunning
	}

	if inspect.State.Paused {
		return nil, ErrPaused
	}

	if inspect.NetworkSettings == nil {
		return nil, ErrNoNetwork
	}
	ip := desktopAddress(sanitizeName(rawID), inspect.NetworkSettings.Networks)
	if ip == "" {
		return nil, ErrNoAddress
	}

	target := &shotTarget{addr: net.JoinHostPort(ip, vncPort), hostname: name, until: time.Now().Add(shotTargetTTL)}
	if inspect.ContainerJSONBase != nil {
		target.containerID = inspect.ID
	}
	if inspect.Config != nil {
		target.password = envValue(inspect.Config.Env, "VNC_PW")
		if inspect.Config.Hostname != "" {
			target.hostname = inspect.Config.Hostname
		}
	}
	return target, nil
}

// captureFrame opens one RFB session against a desktop's Xvnc and returns its
// full screen as an image. The session is deliberately short-lived: connect,
// authenticate, ask for one non-incremental update, read it, hang up.
func captureFrame(ctx context.Context, addr, password string) (*image.RGBA, string, error) {
	dialer := net.Dialer{
		Timeout: 2 * time.Second,
	}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("connect to desktop screen: %w", err)
	}

	defer conn.Close()

	deadline := time.Now().Add(captureTimeout)

	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return nil, "", err
	}

	reader := bufio.NewReaderSize(conn, 64*1024)

	if err := rfbHandshake(reader, conn, password); err != nil {
		return nil, "", err
	}

	width, height, name, err := rfbInit(reader, conn)
	if err != nil {
		return nil, "", err
	}

	frame, err := rfbReadFrame(reader, conn, width, height)
	return frame, name, err
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
func rfbInit(reader io.Reader, writer io.Writer) (width, height int, name string, err error) {
	// shared=1: never disconnect whoever is already looking at this desktop.
	// A thumbnail must be invisible to the human using the screen.
	if _, err := writer.Write([]byte{1}); err != nil {
		return 0, 0, "", err
	}

	header := make([]byte, 24)

	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, 0, "", fmt.Errorf("read desktop size: %w", err)
	}

	width = int(binary.BigEndian.Uint16(header[0:2]))
	height = int(binary.BigEndian.Uint16(header[2:4]))

	nameLength := binary.BigEndian.Uint32(header[20:24])

	// The screen's desktop name (TigerVNC: "<hostname>:<display> (<user>)"): what Screenshot checks a cached address
	// against. Kept to 1 KiB; a longer one is read and dropped.
	if nameLength > 0 {
		keep := min(nameLength, 1024)
		raw := make([]byte, keep)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return 0, 0, "", fmt.Errorf("read desktop name: %w", err)
		}
		name = string(raw)
		if nameLength > keep {
			if _, err := io.CopyN(io.Discard, reader, int64(nameLength-keep)); err != nil {
				return 0, 0, "", fmt.Errorf("read desktop name: %w", err)
			}
		}
	}

	if width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, "", fmt.Errorf("desktop reported an unusable screen size %dx%d", width, height)
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
		return 0, 0, "", err
	}

	// Offer Raw only. Anything else would need a decoder here, and a
	// thumbnail is not worth carrying one.
	setEncodings := make([]byte, 8)
	setEncodings[0] = msgSetEncodings
	binary.BigEndian.PutUint16(setEncodings[2:4], 1)
	binary.BigEndian.PutUint32(setEncodings[4:8], uint32(encodingRaw))

	if _, err := writer.Write(setEncodings); err != nil {
		return 0, 0, "", err
	}

	return width, height, name, nil
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
