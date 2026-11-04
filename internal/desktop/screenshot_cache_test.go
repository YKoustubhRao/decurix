package desktop

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/jpeg"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// fakeRFB is a VNC server that answers one framebuffer request per connection with a w x h screen of one colour,
// counting its sessions.
type fakeRFB struct {
	sessions atomic.Int32
	colour   atomic.Uint32
	w, h     int
}

func (f *fakeRFB) serve(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
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

func (f *fakeRFB) session(conn net.Conn) {
	defer conn.Close()
	f.sessions.Add(1)
	r := conn
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
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:2], uint16(f.w))
	binary.BigEndian.PutUint16(init[2:4], uint16(f.h))
	_, _ = conn.Write(init)
	if !discard(20 + 8 + 10) { // SetPixelFormat, SetEncodings (one), FramebufferUpdateRequest
		return
	}
	var update bytes.Buffer
	update.Write([]byte{0, 0, 0, 1})
	rect := make([]byte, 12)
	binary.BigEndian.PutUint16(rect[4:6], uint16(f.w))
	binary.BigEndian.PutUint16(rect[6:8], uint16(f.h))
	update.Write(rect)
	px := make([]byte, 4)
	binary.LittleEndian.PutUint32(px, f.colour.Load())
	for i := 0; i < f.w*f.h; i++ {
		update.Write(px)
	}
	_, _ = conn.Write(update.Bytes())
	_, _ = io.Copy(io.Discard, conn)
}

// shotManager is a Manager whose running desktop "box" has its VNC at the fake server, counting inspects and disk
// writes of thumbnails.
func shotManager(t *testing.T, rfb *fakeRFB) (*Manager, *fakeAPI, *atomic.Int32) {
	t.Helper()
	t.Setenv("DECURIX_DATA_DIR", t.TempDir())
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
	expire := func() {
		e := m.shotCacheFor("box")
		e.mu.Lock()
		e.at = time.Now().Add(-time.Hour)
		e.mu.Unlock()
	}
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
