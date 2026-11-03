package phoneagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/phonehost"
)

// The phone side runs scrcpy's own server (Apache 2.0, Genymobile) on the
// phone through ADB: it has the shell user's rights, which is what creating a
// display and injecting input need. Its protocol is internal to scrcpy and
// changes between versions, so the version is pinned and every byte layout
// below was read from that version's source (app/src/control_msg.c,
// doc/develop.md, server/.../Streamer.java, util/LogUtils.java at v4.1).
const (
	ServerVersion = "4.1"
	serverURL     = "https://github.com/Genymobile/scrcpy/releases/download/v4.1/scrcpy-server-v4.1"
	// serverSHA256 is GitHub's own digest for that release asset.
	serverSHA256 = "deacb991ed2509715160ffdc7907e47b4160eb30d1566217e9047fd5b8850cae"
	// serverOnPhone is where the jar is pushed; app_process loads it from there.
	serverOnPhone = "/data/local/tmp/decurix-scrcpy-" + ServerVersion + ".jar"

	// deviceNameLength is the device-meta field sent first on the video socket.
	deviceNameLength = 64
	// frameHeaderLength is scrcpy's header before every video packet.
	frameHeaderLength = 12
	// codecH264 is the codec id the video socket opens with.
	codecH264 = 0x68323634
)

// Control message types (enum sc_control_msg_type, v4.1).
const (
	msgInjectKeycode     = 0
	msgInjectText        = 1
	msgInjectTouch       = 2
	msgInjectScroll      = 3
	msgBackOrScreenOn    = 4
	msgStartApp          = 16
	pointerGenericFinger = math.MaxUint64 - 1 // SC_POINTER_ID_GENERIC_FINGER, UINT64_C(-2)
	maxInjectText        = 300
)

// ensureServer returns the path of the pinned scrcpy-server jar in dir,
// downloading it once and refusing anything whose hash is not the release's.
func ensureServer(ctx context.Context, dir string) (string, error) {
	path := filepath.Join(dir, "scrcpy-server-v"+ServerVersion)

	if raw, err := os.ReadFile(path); err == nil {
		if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) == serverSHA256 {
			return path, nil
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading scrcpy-server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading scrcpy-server: %s", resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}

	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != serverSHA256 {
		return "", errors.New("the downloaded scrcpy-server is not the release it should be (hash mismatch); not using it")
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}

	return path, nil
}

// serverArgs is the shell command that runs the pushed server with args.
func serverArgs(args ...string) []string {
	return append([]string{"shell", "CLASSPATH=" + serverOnPhone, "app_process", "/", "com.genymobile.scrcpy.Server", ServerVersion}, args...)
}

// parseAppList reads the server's list_apps output:
//
//	[server] INFO: List of apps:
//	 * Camera                         com.android.camera
//	 - A name longer than thirty characters
//	                                 com.example.app
//
// "*" marks a system app. A name of 30 characters or more puts the package on
// the next line.
func parseAppList(out string) []phonehost.App {
	var apps []phonehost.App
	var pendingName string
	var pendingSystem bool

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(line, " * ") || strings.HasPrefix(line, " - ") {
			pendingName, pendingSystem = "", false
			rest := line[3:]
			system := line[1] == '*'

			// "Name<padding> package": the padding and the space after it are at
			// least two spaces, which a name ("Foo v1.2") never ends in.
			if i := strings.LastIndex(rest, "  "); i > 0 && strings.TrimSpace(rest[:i]) != "" && looksLikePackage(strings.TrimSpace(rest[i:])) {
				apps = append(apps, phonehost.App{Name: strings.TrimSpace(rest[:i]), Package: strings.TrimSpace(rest[i:]), System: system})
				continue
			}

			pendingName, pendingSystem = strings.TrimSpace(rest), system
			continue
		}

		if pendingName != "" && looksLikePackage(trimmed) {
			apps = append(apps, phonehost.App{Name: pendingName, Package: trimmed, System: pendingSystem})
			pendingName = ""
		}
	}

	return apps
}

// looksLikePackage reports whether s is shaped like an Android package name.
func looksLikePackage(s string) bool {
	if !strings.Contains(s, ".") || strings.ContainsAny(s, " \t/") {
		return false
	}

	for _, r := range s {
		if !(r == '.' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}

	return true
}

// encodeInput turns one viewer event into scrcpy control messages.
func encodeInput(in phonehost.Input) ([][]byte, error) {
	switch in.Kind {
	case "touch":
		b := make([]byte, 32)
		b[0] = msgInjectTouch
		b[1] = byte(in.Action)
		binary.BigEndian.PutUint64(b[2:], pointerGenericFinger)
		putPosition(b[10:], in)
		if in.Action != 1 { // no pressure on ACTION_UP
			binary.BigEndian.PutUint16(b[22:], 0xffff)
		}
		// action_button and buttons stay 0: a finger, not a mouse.
		return [][]byte{b}, nil

	case "scroll":
		b := make([]byte, 21)
		b[0] = msgInjectScroll
		putPosition(b[1:], in)
		binary.BigEndian.PutUint16(b[13:], uint16(scrollFixed(in.ScrollX)))
		binary.BigEndian.PutUint16(b[15:], uint16(scrollFixed(in.ScrollY)))
		return [][]byte{b}, nil

	case "key":
		// The control plane refuses system keys already (phonehost.OrdinaryKey);
		// the phone refuses them too, whoever sent them.
		if !phonehost.OrdinaryKey(in.Key) {
			return nil, fmt.Errorf("key %d is not a typing or navigation key", in.Key)
		}
		b := make([]byte, 14)
		b[0] = msgInjectKeycode
		b[1] = byte(in.Action)
		binary.BigEndian.PutUint32(b[2:], uint32(in.Key))
		binary.BigEndian.PutUint32(b[10:], uint32(in.Meta))
		return [][]byte{b}, nil

	case "text":
		var out [][]byte
		for _, chunk := range splitText(in.Text, maxInjectText) {
			b := make([]byte, 5+len(chunk))
			b[0] = msgInjectText
			binary.BigEndian.PutUint32(b[1:], uint32(len(chunk)))
			copy(b[5:], chunk)
			out = append(out, b)
		}
		return out, nil

	case "back":
		return [][]byte{{msgBackOrScreenOn, 0}, {msgBackOrScreenOn, 1}}, nil
	}

	return nil, fmt.Errorf("unknown input %q", in.Kind)
}

// putPosition writes scrcpy's position: x, y (int32), then the picture's
// width and height (uint16), 12 bytes.
func putPosition(b []byte, in phonehost.Input) {
	binary.BigEndian.PutUint32(b[0:], uint32(int32(in.X)))
	binary.BigEndian.PutUint32(b[4:], uint32(int32(in.Y)))
	binary.BigEndian.PutUint16(b[8:], uint16(in.W))
	binary.BigEndian.PutUint16(b[10:], uint16(in.H))
}

// scrollFixed is sc_float_to_i16fp of a scroll amount in [-16, 16].
func scrollFixed(v float64) int16 {
	v = math.Max(-1, math.Min(1, v/16))
	if v >= 1 {
		return math.MaxInt16
	}

	return int16(v * 32768)
}

// splitText cuts s into pieces of at most max bytes without splitting a character.
func splitText(s string, max int) []string {
	var out []string

	for len(s) > max {
		cut := max
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		if cut == 0 {
			// No character starts in the first max bytes (bytes that are not
			// UTF-8): cut at max, or this would never end.
			cut = max
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}

	if s != "" {
		out = append(out, s)
	}

	return out
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// startAppMessage is the control message that opens pkg on the session's display.
func startAppMessage(pkg string) []byte {
	if len(pkg) > 255 {
		pkg = pkg[:255]
	}

	return append([]byte{msgStartApp, byte(len(pkg))}, pkg...)
}

// scid is a random 31-bit session id, hex as the server parses it.
func newSCID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%08x", binary.BigEndian.Uint32(b[:])&0x7fffffff)
}

// freePort returns a local TCP port nothing is listening on.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}

// dialForwarded connects to an adb-forwarded port and reads the dummy byte
// the server sends first on a forward tunnel. Until the server listens, adb
// accepts the connection and closes it at once, so it retries until ctx ends.
func dialForwarded(ctx context.Context, port int, wantDummy bool) (net.Conn, *bufio.Reader, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			br := bufio.NewReaderSize(conn, 64<<10)
			if !wantDummy {
				return conn, br, nil
			}

			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err = br.ReadByte(); err == nil {
				conn.SetReadDeadline(time.Time{})
				return conn, br, nil
			}
			conn.Close()
		}

		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("the phone's app server did not start: %w", ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// maxVideoPacket is the largest video payload one link message can carry:
// the control plane's WebSocket reader (internal/wsconn) takes at most 4 MiB
// in one message, and a frame goes out with a 4-byte session prefix and its
// header. A larger frame would have closed the whole link; now it ends only
// its session, saying why (review 2026-11-03, B-37).
const maxVideoPacket = 4<<20 - 4 - frameHeaderLength

// errFrameTooLarge is a video frame too large for the link.
var errFrameTooLarge = errors.New("a video frame was too large to send; open the app again at a smaller size")

// readFrame reads one scrcpy video message: a 12-byte header, and for a
// media packet its payload. It returns both together, as they are sent on.
func readFrame(r io.Reader) ([]byte, error) {
	var head [frameHeaderLength]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}

	// A session packet (the display's size) has its top bit set and no payload.
	if head[0]&0x80 != 0 {
		return append([]byte(nil), head[:]...), nil
	}

	size := binary.BigEndian.Uint32(head[8:])
	if size > maxVideoPacket {
		return nil, errFrameTooLarge
	}

	msg := make([]byte, frameHeaderLength+int(size))
	copy(msg, head[:])
	if _, err := io.ReadFull(r, msg[frameHeaderLength:]); err != nil {
		return nil, err
	}

	return msg, nil
}

// adbCommand builds an adb command for the phone serial (any device when "").
func adbCommand(ctx context.Context, serial string, args ...string) *exec.Cmd {
	if serial != "" {
		args = append([]string{"-s", serial}, args...)
	}

	return exec.CommandContext(ctx, "adb", args...)
}
