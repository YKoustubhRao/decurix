package desktop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"image/jpeg"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// fakeRFB is a VNC server whose screen is a w x h field of one colour. A session stays open: it answers a
// non-incremental request with the whole screen at once, and an incremental one only once the colour has changed
// since it last sent it (as Xvnc holds an incremental request until something changes). It counts sessions, the
// sessions that ended, the updates and bytes it sent, and the requests it was asked that were not incremental.
type fakeRFB struct {
	sessions atomic.Int32
	ended    atomic.Int32
	updates  atomic.Int32
	sent     atomic.Int64 // bytes of updates sent
	full     atomic.Int32 // non-incremental requests
	colour   atomic.Uint32
	w, h     int
	// name is the screen's RFB desktop name (a string); unset or "" for "desktop_box:1 (headless)".
	name atomic.Value
	// refuse closes each connection at once, as a screen that went away.
	refuse atomic.Bool

	mu   sync.Mutex
	live map[net.Conn]bool
}

func (f *fakeRFB) serve(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); f.kill() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.session(conn)
		}
	}()
	return ln.Addr().String()
}

// kill closes every open session, as a screen that went away.
func (f *fakeRFB) kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.live {
		_ = c.Close()
	}
}

func (f *fakeRFB) session(conn net.Conn) {
	defer conn.Close()
	if f.refuse.Load() {
		return
	}
	f.sessions.Add(1)
	f.mu.Lock()
	if f.live == nil {
		f.live = map[net.Conn]bool{}
	}
	f.live[conn] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.live, conn)
		f.mu.Unlock()
		f.ended.Add(1)
	}()
	r := bufio.NewReader(conn)
	discard := func(n int) bool { _, err := io.CopyN(io.Discard, r, int64(n)); return err == nil }
	_, _ = conn.Write([]byte("RFB 003.008\n"))
	if !discard(12) {
		return
	}
	_, _ = conn.Write([]byte{1, 1}) // one security type: None
	if !discard(1) {
		return
	}
	_, _ = conn.Write([]byte{0, 0, 0, 0}) // security result: OK
	if !discard(1) {                      // ClientInit
		return
	}
	name, _ := f.name.Load().(string)
	if name == "" {
		name = "desktop_box:1 (headless)" // TigerVNC's desktop name: the container's hostname first
	}
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:2], uint16(f.w))
	binary.BigEndian.PutUint16(init[2:4], uint16(f.h))
	binary.BigEndian.PutUint32(init[20:24], uint32(len(name)))
	_, _ = conn.Write(append(init, name...))

	// The client's messages, read as they come; each update request goes to the writer.
	requests := make(chan bool, 16) // incremental?
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			kind, err := r.ReadByte()
			if err != nil {
				return
			}
			switch kind {
			case 0: // SetPixelFormat
				if !discard(19) {
					return
				}
			case 2: // SetEncodings
				var h [3]byte
				if _, err := io.ReadFull(r, h[:]); err != nil {
					return
				}
				if !discard(4 * int(binary.BigEndian.Uint16(h[1:3]))) {
					return
				}
			case 3: // FramebufferUpdateRequest
				var req [9]byte
				if _, err := io.ReadFull(r, req[:]); err != nil {
					return
				}
				requests <- req[0] == 1
			default:
				return
			}
		}
	}()
	sentColour, sentAny := uint32(0), false
	for {
		var incremental bool
		select {
		case incremental = <-requests:
		case <-gone:
			return
		}
		if !incremental {
			f.full.Add(1)
		}
		for incremental && sentAny && f.colour.Load() == sentColour {
			select {
			case <-gone:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		colour := f.colour.Load()
		var update bytes.Buffer
		update.Write([]byte{0, 0, 0, 1})
		rect := make([]byte, 12)
		binary.BigEndian.PutUint16(rect[4:6], uint16(f.w))
		binary.BigEndian.PutUint16(rect[6:8], uint16(f.h))
		update.Write(rect)
		px := make([]byte, 4)
		binary.LittleEndian.PutUint32(px, colour)
		for i := 0; i < f.w*f.h; i++ {
			update.Write(px)
		}
		if _, err := conn.Write(update.Bytes()); err != nil {
			return
		}
		sentColour, sentAny = colour, true
		f.updates.Add(1)
		f.sent.Add(int64(update.Len()))
	}
}

// waitUpdates waits until the fake has sent n updates, and the session has painted the last.
func waitUpdates(t *testing.T, m *Manager, f *fakeRFB, n int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.updates.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the screen sent %d updates, want %d", f.updates.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	sess := m.shotCacheFor("box").session.Load()
	for sess != nil {
		sess.mu.Lock()
		v := sess.version
		sess.mu.Unlock()
		if v >= uint64(n) || time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// endSession ends desktop id's screen session as the idle watcher does: the cached address stays.
func endSession(m *Manager, id string) {
	if sess := m.shotCacheFor(id).session.Load(); sess != nil {
		sess.close(errSessionIdle)
	}
}

// shotManager is a Manager whose running desktop "box" has its VNC at the fake server, counting inspects and disk
// writes of thumbnails.
func shotManager(t *testing.T, rfb *fakeRFB) (*Manager, *fakeAPI, *atomic.Int32) {
	t.Helper()
	t.Setenv("DECURIX_DATA_DIR", t.TempDir())
	savedGap, savedIdle := sessionGap, sessionIdle
	sessionGap, sessionIdle = time.Millisecond, time.Hour
	t.Cleanup(func() { sessionGap, sessionIdle = savedGap, savedIdle })
	host, port, _ := net.SplitHostPort(rfb.serve(t))
	saved := vncPort
	vncPort = port
	t.Cleanup(func() { vncPort = saved })
	fake := &fakeAPI{}
	fake.inspect = func(string) (container.InspectResponse, error) {
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true}},
			Config:            &container.Config{},
			NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
				NetworkName: {IPAddress: host},
			}},
		}, nil
	}
	var saves atomic.Int32
	savedSave := saveShot
	saveShot = func(string, []byte) { saves.Add(1) }
	t.Cleanup(func() { saveShot = savedSave })
	return newManagerWith(fake), fake, &saves
}

func widthOf(t *testing.T, data []byte) int {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width
}

// A thumbnail is captured once per TTL whatever the viewers, kept per width, captured again after the TTL without
// another container inspect, written to disk only when it changed, and asks Docker again after a lifecycle change
// (resource plan P1 #4).
func TestThumbnailsAreSharedPerWidthAndCostNoInspectEachCapture(t *testing.T) {
	rfb := &fakeRFB{w: 64, h: 32}
	rfb.colour.Store(0x00ff0000)
	m, fake, saves := shotManager(t, rfb)
	ctx := context.Background()

	first, err := m.Screenshot(ctx, "box", 48)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := m.Screenshot(ctx, "box", 48)
	if !bytes.Equal(first, again) || rfb.sessions.Load() != 1 {
		t.Fatalf("a second viewer inside the TTL cost a capture (%d sessions)", rfb.sessions.Load())
	}
	wide, err := m.Screenshot(ctx, "box", 64)
	if err != nil {
		t.Fatal(err)
	}
	if widthOf(t, first) != 48 || widthOf(t, wide) != 64 {
		t.Fatalf("widths %d and %d, want 48 and 64: the cache gave one viewer another's size", widthOf(t, first), widthOf(t, wide))
	}
	inspects := len(fake.called("ContainerInspect"))
	if inspects != 1 {
		t.Fatalf("%d inspects for captures within the target's life, want 1", inspects)
	}

	// After the TTL: captured again, the address reused, and the same picture not written again.
	expire := func() { ageShots(m, "box", time.Hour) }
	expire()
	before := saves.Load()
	if _, err := m.Screenshot(ctx, "box", 64); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.called("ContainerInspect")); got != 1 {
		t.Fatalf("a capture after the TTL inspected again (%d inspects)", got)
	}
	if saves.Load() != before {
		t.Fatalf("an unchanged screen was written to disk again (%d -> %d)", before, saves.Load())
	}

	// A changed screen is written; a lifecycle change makes the next capture ask Docker.
	rfb.colour.Store(0x000000ff)
	waitUpdates(t, m, rfb, 2)
	expire()
	if _, err := m.Screenshot(ctx, "box", 64); err != nil {
		t.Fatal(err)
	}
	if saves.Load() == before {
		t.Fatal("a changed screen was not written to disk")
	}
	m.InvalidateProxyTarget("box")
	expire()
	if _, err := m.Screenshot(ctx, "box", 64); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.called("ContainerInspect")); got != 2 {
		t.Fatalf("after a lifecycle change the capture did not ask Docker (%d inspects)", got)
	}
}

// ageShots makes every width's cached capture of desktop id older by d.
func ageShots(m *Manager, id string, d time.Duration) {
	e := m.shotCacheFor(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	for w, f := range e.byWidth {
		f.at = f.at.Add(-d)
		e.byWidth[w] = f
	}
}

// centreRed is the red of a thumbnail's centre pixel.
func centreRed(t *testing.T, data []byte) uint32 {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	r, _, _, _ := img.At(b.Dx()/2, b.Dy()/2).RGBA()
	return r >> 8
}

// A capture for one width never keeps another width's older frame alive: each width's frame has its own time
// (review RES-CP-1, the reviewer's case: a width-48 viewer was served a frame ~8.9 s old because a width-64 capture
// reset a shared time).
func TestAnotherWidthNeverExtendsAStaleFrame(t *testing.T) {
	rfb := &fakeRFB{w: 64, h: 32}
	rfb.colour.Store(0x00ff0000) // red
	m, _, _ := shotManager(t, rfb)
	ctx := context.Background()
	first, err := m.Screenshot(ctx, "box", 48)
	if err != nil {
		t.Fatal(err)
	}
	rfb.colour.Store(0x000000ff) // the screen changes to blue
	waitUpdates(t, m, rfb, 2)
	ageShots(m, "box", 4*time.Second)
	if _, err := m.Screenshot(ctx, "box", 64); err != nil { // another viewer, inside the TTL
		t.Fatal(err)
	}
	ageShots(m, "box", 1100*time.Millisecond) // width 48's frame is now 5.1 s old; width 64's 1.1 s
	again, err := m.Screenshot(ctx, "box", 48)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(again, first) || centreRed(t, again) > 100 {
		t.Fatalf("width 48 was served its frame from more than a sweep ago (red %d)", centreRed(t, again))
	}
}

// A failed capture drops the cached address, so the next capture asks Docker; a cached address that reaches a screen
// naming another container is refused and dropped; and a screen that names no container at all is checked by Docker
// on every capture (review RES-CP-1).
func TestTheCachedScreenAddressIsCheckedAndDropped(t *testing.T) {
	rfb := &fakeRFB{w: 32, h: 16}
	m, fake, _ := shotManager(t, rfb)
	ctx := context.Background()
	if _, err := m.Screenshot(ctx, "box", 32); err != nil {
		t.Fatal(err)
	}
	inspects := func() int { return len(fake.called("ContainerInspect")) }

	// Another container now answers at the cached address: refused, never shown as box's.
	rfb.name.Store("desktop_other:1 (headless)")
	endSession(m, "box")
	ageShots(m, "box", time.Hour)
	if _, err := m.Screenshot(ctx, "box", 32); err == nil {
		t.Fatal("a screen naming another container was shown as this desktop's")
	}
	if m.shotCacheFor("box").target.Load() != nil {
		t.Fatal("the address that reached another screen is still cached")
	}

	// A failed capture drops the address: the next capture inspects.
	rfb.name.Store("")
	before := inspects()
	m.shotCacheFor("box").lastErr = nil
	if _, err := m.Screenshot(ctx, "box", 32); err != nil {
		t.Fatal(err)
	}
	if inspects() != before+1 {
		t.Fatalf("after a failure the capture did not ask Docker (%d -> %d inspects)", before, inspects())
	}

	// A screen that names no container: every capture asks Docker.
	rfb.name.Store("x11")
	m2, fake2, _ := shotManager(t, rfb)
	for i := 0; i < 3; i++ {
		endSession(m2, "box")
		ageShots(m2, "box", time.Hour)
		if _, err := m2.Screenshot(ctx, "box", 32); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(fake2.called("ContainerInspect")); n != 3 {
		t.Fatalf("a nameless screen's sessions made %d inspects, want one each (3)", n)
	}
}

// Pausing a desktop, here or around a home copy or a tar out, drops its cached screen address.
func TestAPauseDropsTheCachedScreenAddress(t *testing.T) {
	rfb := &fakeRFB{w: 32, h: 16}
	m, _, _ := shotManager(t, rfb)
	if _, err := m.Screenshot(context.Background(), "box", 32); err != nil {
		t.Fatal(err)
	}
	sess := m.shotCacheFor("box").session.Load()
	m.forgetShotTargetOf("cid-box") // a pause by container ID (homefs, exectar)
	if m.shotCacheFor("box").target.Load() != nil || m.shotCacheFor("box").session.Load() != nil || sess == nil || sess.alive() {
		t.Fatal("a pause by container ID kept the address or the screen session")
	}
	if _, err := m.Screenshot(context.Background(), "box", 48); err != nil {
		t.Fatal(err)
	}
	m.forgetShotTargetOf("desktop_box") // by container name
	if m.shotCacheFor("box").target.Load() != nil {
		t.Fatal("a pause by container name kept the address")
	}
}

// A capture that fails at the screen (the connection closes) drops the cached address, so the next capture asks
// Docker again (review RES-CP-1 test gap).
func TestAFailedCaptureDropsTheCachedAddress(t *testing.T) {
	rfb := &fakeRFB{w: 32, h: 16}
	m, fake, _ := shotManager(t, rfb)
	ctx := context.Background()
	if _, err := m.Screenshot(ctx, "box", 32); err != nil {
		t.Fatal(err)
	}
	inspects := len(fake.called("ContainerInspect"))
	rfb.refuse.Store(true)
	endSession(m, "box")
	ageShots(m, "box", time.Hour)
	if _, err := m.Screenshot(ctx, "box", 32); err == nil {
		t.Fatal("a screen that closed the connection gave a thumbnail")
	}
	if len(fake.called("ContainerInspect")) != inspects {
		t.Fatal("the failing capture did not use the cached address")
	}
	rfb.refuse.Store(false)
	m.shotCacheFor("box").lastErr = nil
	if _, err := m.Screenshot(ctx, "box", 32); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.called("ContainerInspect")); got != inspects+1 {
		t.Fatalf("after the failure the capture made %d inspects, want one more (%d)", got, inspects+1)
	}
}

// One session serves every capture while the desktop is watched: after the first whole screen, the screen is asked
// for incrementally only, and a screen that does not change sends nothing and is not encoded again (Phase 2 C).
func TestAnUnchangedScreenCostsNothing(t *testing.T) {
	rfb := &fakeRFB{w: 64, h: 32}
	rfb.colour.Store(0x00ff0000)
	m, fake, saves := shotManager(t, rfb)
	ctx := context.Background()
	first, err := m.Screenshot(ctx, "box", 48)
	if err != nil {
		t.Fatal(err)
	}
	// The fake counts an update after writing it, so the first may be painted before it is counted: counted first, or
	// the whole screen is taken for a cost of the unchanged ones (CI flake, 2026-11-09).
	waitUpdates(t, m, rfb, 1)
	sent, updates, before := rfb.sent.Load(), rfb.updates.Load(), saves.Load()
	for i := 0; i < 5; i++ {
		ageShots(m, "box", time.Hour)
		time.Sleep(5 * time.Millisecond) // the session's next incremental request is out
		again, err := m.Screenshot(ctx, "box", 48)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, first) {
			t.Fatal("an unchanged screen gave another thumbnail")
		}
	}
	if rfb.sent.Load() != sent || rfb.updates.Load() != updates {
		t.Fatalf("an unchanged screen cost %d bytes in %d updates", rfb.sent.Load()-sent, rfb.updates.Load()-updates)
	}
	if rfb.sessions.Load() != 1 || rfb.full.Load() != 1 || len(fake.called("ContainerInspect")) != 1 || saves.Load() != before {
		t.Fatalf("%d sessions, %d whole-screen requests, %d inspects, %d saves: want one session, one whole screen, one inspect, no save",
			rfb.sessions.Load(), rfb.full.Load(), len(fake.called("ContainerInspect")), saves.Load()-before)
	}

	// A change comes as an incremental update on the same session.
	rfb.colour.Store(0x000000ff)
	waitUpdates(t, m, rfb, updates+1)
	ageShots(m, "box", time.Hour)
	changed, err := m.Screenshot(ctx, "box", 48)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(changed, first) || centreRed(t, changed) > 100 || rfb.sessions.Load() != 1 || rfb.full.Load() != 1 {
		t.Fatalf("the changed screen: red %d, %d sessions, %d whole-screen requests", centreRed(t, changed), rfb.sessions.Load(), rfb.full.Load())
	}
}

// A session no thumbnail asks of is closed after sessionIdle, and the next capture opens another on the cached
// address, with no inspect.
func TestAnIdleScreenSessionCloses(t *testing.T) {
	rfb := &fakeRFB{w: 32, h: 16}
	m, fake, _ := shotManager(t, rfb)
	savedIdle, savedCheck := sessionIdle, sessionIdleCheck
	sessionIdle, sessionIdleCheck = 40*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sessionIdle, sessionIdleCheck = savedIdle, savedCheck })
	if _, err := m.Screenshot(context.Background(), "box", 32); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for rfb.ended.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("an unwatched screen session stayed open")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ageShots(m, "box", time.Hour)
	if _, err := m.Screenshot(context.Background(), "box", 32); err != nil {
		t.Fatal(err)
	}
	if rfb.sessions.Load() != 2 || len(fake.called("ContainerInspect")) != 1 {
		t.Fatalf("%d sessions, %d inspects: want a second session on the cached address", rfb.sessions.Load(), len(fake.called("ContainerInspect")))
	}
}
