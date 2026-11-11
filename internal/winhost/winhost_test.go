package winhost

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOpts shapes the stand-in for bridge.ps1.
type fakeOpts struct {
	launch string // the answer to launch; "OK pid=42" if empty
	noWin  bool   // launch succeeds but no window ever shows, and status says the app exited
	moved  bool   // a launchr of an already-resolved program says it is no longer there
	slow   string // seconds a launch takes to answer (a first browser start importing the profile), "" for none
	held   bool   // dropprofiles answers that a profile is still held open
}

// requireBash skips a test that stands a bash script in for the Windows helper. The protocol is the same on every OS, but
// a Windows machine has no bash to answer it (or, with WSL installed, one that runs inside Linux).
func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helper is faked with bash")
	}
}

// fakeHelper stands in for bridge.ps1: the same line protocol, answered by a bash loop, and pictures sent the way the real
// one sends them (a header line, then the JPEG bytes). Every line it receives is appended to the file named by log, so a
// test can see exactly what reached the host. Besides the real verbs it understands two for tests:
//
//	emit <whole 0|1> <x> <y> <w> <h> <payload>   reply OK, then send that patch of a 20x10 picture
//	close                                       reply OK, then say the app has no window left
func fakeHelper(t *testing.T, o fakeOpts) (Starter, *int32) {
	t.Helper()
	requireBash(t)
	if o.launch == "" {
		o.launch = "OK pid=42"
	}
	log := t.TempDir() + "/received.log"
	t.Setenv("FAKE_LOG", log)
	noWin := "0"
	if o.noWin {
		noWin = "1"
	}
	moved := "0"
	if o.moved {
		moved = "1"
	}
	held := "0"
	if o.held {
		held = "1"
	}
	script := `
dolaunch() { [ -n "` + o.slow + `" ] && sleep ` + o.slow + `; echo "` + o.launch + `"; if [ "` + noWin + `" = 0 ] && [[ "` + o.launch + `" == OK* ]]; then send 1 0 0 20 10 ABC; fi; }
send() { printf 'FRAME 20 10 %s %s %s %s %s %s\n%s' "$2" "$3" "$4" "$5" "$1" "${#6}" "$6"; }
while IFS= read -r l; do
  printf '%s\n' "$l" >> "$FAKE_LOG"
  printf '%s %s\n' "$$" "$l" >> "$FAKE_LOG.byhelper"
  case "$l" in
    launchr*) if [ "` + moved + `" = 1 ]; then echo "ERR Not found on this computer: gone"; else dolaunch; fi;;
    launch*)  dolaunch;;
    keyframe) echo OK; send 1 0 0 20 10 ABC;;
    "thumb 4") if [ -z "$thumbed" ]; then thumbed=1; printf 'THUMB 4 2 3
XYZ'; else echo "THUMB same"; fi;;
    status)   if [ "` + noWin + `" = 1 ]; then echo "STATUS exited 0x9"; else echo "STATUS running"; fi;;
    emit*)    set -- $l; echo OK; send "$2" "$3" "$4" "$5" "$6" "$7";;
    close)    echo OK; echo "EVENT closed";;
    "copy 67") echo "CLIP aGVsbG8gd29ybGQ=";;
    "copy 88") echo "CLIP";;
    check*uwp*) echo "ERR This is a Universal Windows (UWP) app";;
    check*webapp*) printf 'OK\tC:\\Edge\\msedge.exe\t--app=http://localhost:8000\t\t1\tmsedge\tC:\\webapp.exe\n';;
    check*)   IFS=$'\t' read -r _ t a <<< "$l"; printf 'OK\t%s\t%s\t\t0\tx\n' "$t" "$a";;
    pid)      echo "OK 4242";;
    dropprofiles*) if [ "` + held + `" = 1 ]; then echo "OK removed=0 left=1 locked=2"; else echo "OK removed=1 left=0 locked=0"; fi;;
    quit)     exit 0;;
    *)        echo OK;;
  esac
done`
	var starts int32
	return func(ctx context.Context, name string) (*exec.Cmd, error) {
		atomic.AddInt32(&starts, 1)
		return exec.Command("bash", "-c", script), nil
	}, &starts
}

func received(t *testing.T) []string {
	t.Helper()
	// The helper writes as it goes; give a stopped session a moment to flush.
	time.Sleep(50 * time.Millisecond)
	out, _ := exec.Command("cat", getenv("FAKE_LOG")).Output()
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func getenv(k string) string {
	out, _ := exec.Command("bash", "-c", "printf %s \"$"+k+"\"").Output()
	return string(out)
}

// fakeManager is a Manager whose helpers are fakeHelper's: it is stopped, and its helpers still starting in the
// background are waited for, when the test ends.
func fakeManager(t *testing.T, o fakeOpts) (*Manager, *int32) {
	t.Helper()
	start, starts := fakeHelper(t, o)
	m := NewManager(start)
	t.Cleanup(m.waitIdle)
	t.Cleanup(m.StopAll)
	return m, starts
}

func startFake(t *testing.T, id string) (*Manager, *Session) {
	t.Helper()
	m, _ := fakeManager(t, fakeOpts{})
	s, err := m.Start(context.Background(), id, `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

func next(t *testing.T, v *Viewer) Update {
	t.Helper()
	select {
	case u, ok := <-v.C:
		if !ok {
			t.Fatal("viewer closed")
		}
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("no picture within 3s")
	}
	return Update{}
}

func TestStartWaitsForFirstPictureAndForwardsInputVerbatim(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{launch: "OK pid=42"})

	s, err := m.Start(context.Background(), "vm1-app", `C:\Program Files\App\app.exe`, "--flag")
	if err != nil {
		t.Fatal(err)
	}
	if w, h := s.Size(); w != 20 || h != 10 {
		t.Fatalf("size = %dx%d, want the first picture's 20x10", w, h)
	}

	if err := s.Mouse("down", 10, 20, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Mouse("up", 10, 20, true, "cs"); err != nil {
		t.Fatal(err)
	}
	if err := s.Type("hello world\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Key(13, "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.Wheel(1, 2, -3); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(received(t), "|")
	for _, w := range []string{"desktop decurix_vm1_app", "viewers 1", "launch\tC:\\Program Files\\App\\app.exe\t--flag", "down 10 20", "up 10 20 right +cs", "type hello world", "key 13 c", "wheel 1 2 -3"} {
		if !strings.Contains(got, w) {
			t.Errorf("helper never received %q; got %q", w, got)
		}
	}
}

func TestAViewerGetsTheWholePictureFirstAndThenOnlyWhatChanged(t *testing.T) {
	_, s := startFake(t, "patches")
	v := s.Watch()
	defer v.Close()

	first := next(t, v)
	if !first.Whole || first.W != 20 || first.H != 10 || string(first.JPEG) != "ABC" {
		t.Fatalf("first = %+v, want the whole picture", first)
	}
	if _, err := s.Do("emit 0 4 2 8 6 XY"); err != nil {
		t.Fatal(err)
	}
	patch := next(t, v)
	if patch.Whole || patch.X != 4 || patch.Y != 2 || patch.Rw != 8 || patch.Rh != 6 || string(patch.JPEG) != "XY" {
		t.Fatalf("patch = %+v, want the 8x6 rectangle at 4,2", patch)
	}
}

func TestAViewerThatJoinsLateIsGivenAWholePictureAtOnce(t *testing.T) {
	_, s := startFake(t, "late")
	first := s.Watch()
	next(t, first)
	first.Close()

	// Nothing on the app has changed since, so no picture is coming on its own: the join itself asks for one.
	late := s.Watch()
	defer late.Close()
	if u := next(t, late); !u.Whole {
		t.Fatalf("a viewer's first picture was a patch: %+v", u)
	}
}

func TestPatchesAreNotDrawnOnAPictureTheViewerNeverGot(t *testing.T) {
	s := &Session{viewers: map[*Viewer]struct{}{}, first: make(chan struct{}), done: closedChan()}
	ch := make(chan Update, 4)
	v := &Viewer{C: ch, s: s, ch: ch, waiting: true}
	s.viewers[v] = struct{}{}

	s.publish(Update{W: 20, H: 10, Rw: 2, Rh: 2, JPEG: []byte("p")}) // a patch: useless to a viewer with nothing yet
	if len(ch) != 0 {
		t.Fatal("a patch was delivered to a viewer that has no picture to put it on")
	}
	s.publish(Update{W: 20, H: 10, Rw: 20, Rh: 10, Whole: true, JPEG: []byte("w")})
	s.publish(Update{W: 20, H: 10, Rw: 2, Rh: 2, JPEG: []byte("p")})
	if len(ch) != 2 {
		t.Fatalf("delivered %d, want the whole picture and the patch after it", len(ch))
	}
}

func TestAViewerThatFallsBehindLosesItsBacklogAndWaitsForAWholePicture(t *testing.T) {
	s := &Session{viewers: map[*Viewer]struct{}{}, first: make(chan struct{}), done: closedChan()}
	ch := make(chan Update, 2)
	v := &Viewer{C: ch, s: s, ch: ch}
	s.viewers[v] = struct{}{}

	for i := 0; i < 3; i++ { // one more than fits
		s.publish(Update{W: 20, H: 10, Rw: 2, Rh: 2, JPEG: []byte("p")})
	}
	if len(ch) != 0 || !v.waiting {
		t.Fatalf("backlog %d, waiting %v: a viewer that missed a patch must drop the rest and wait for a whole picture", len(ch), v.waiting)
	}
	s.publish(Update{W: 20, H: 10, Rw: 20, Rh: 10, Whole: true, JPEG: []byte("w")})
	if len(ch) != 1 || v.waiting {
		t.Fatalf("backlog %d, waiting %v: the whole picture should bring the viewer back", len(ch), v.waiting)
	}
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func TestParseFrameHeaderRejectsWhatCouldOverrunTheBuffer(t *testing.T) {
	good := "FRAME 20 10 4 2 8 6 0 3"
	if h, ok := parseFrameHeader(good); !ok || h.n != 3 || h.Whole || h.X != 4 || h.Rw != 8 {
		t.Fatalf("%q = %+v, %v", good, h, ok)
	}
	for _, bad := range []string{
		"FRAME 20 10 4 2 8 6 0",           // too short
		"FRAME 20 10 4 2 8 6 0 3 9",       // too long
		"FRAME a 10 4 2 8 6 0 3",          // not a number
		"FRAME 20 10 4 2 8 6 0 -3",        // negative length
		"FRAME 20 10 4 2 8 6 0 0",         // empty picture
		"FRAME 20 10 4 2 8 6 0 999999999", // absurd length
		"FRAME 20 10 15 2 8 6 0 3",        // rectangle sticks out to the right
		"FRAME 20 10 4 8 8 6 0 3",         // ...and the bottom
		"FRAME 0 10 0 0 0 6 0 3",          // no width
		"FRAME 20 10 4 2 0 6 0 3",         // empty rectangle
		"OTHER 20 10 4 2 8 6 0 3",         // not a picture
	} {
		if _, ok := parseFrameHeader(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// A launch that answers after the ordinary request timeout - the first start of a browser in a VM, which copies the
// person's profile first - must still start the app, not be killed as a helper that stopped answering.
func TestALaunchMayTakeLongerThanAnOrdinaryRequest(t *testing.T) {
	start, _ := fakeHelper(t, fakeOpts{slow: "1"})
	defer func(d time.Duration) { requestTimeout = d }(requestTimeout)
	requestTimeout = 300 * time.Millisecond
	m := NewManager(start)
	defer m.StopAll()
	s, err := m.Start(context.Background(), "vm-chrome", `C:\chrome.exe`, "")
	if err != nil {
		t.Fatalf("a slow launch failed: %v", err)
	}
	if w, h := s.Size(); w != 20 || h != 10 {
		t.Fatalf("size %dx%d after a slow launch", w, h)
	}
	m.waitIdle()
}

func TestStartReturnsTheHelpersReasonWhenTheLaunchIsRefused(t *testing.T) {
	m, starts := fakeManager(t, fakeOpts{launch: "ERR This is a Microsoft Store app."})
	_, err := m.Start(context.Background(), "vm1-calc", `shell:AppsFolder\X`, "")
	if err == nil || !strings.Contains(err.Error(), "Microsoft Store app") {
		t.Fatalf("err = %v, want the helper's own reason", err)
	}

	// The helper ran nothing, so it is still a good warm helper: the next app should not pay to start another.
	if _, err := m.Start(context.Background(), "vm1-calc", `shell:AppsFolder\X`, ""); err == nil {
		t.Fatal("second start succeeded")
	}
	if n := atomic.LoadInt32(starts); n != 1 {
		t.Errorf("%d helpers started for two refused launches, want the same one reused", n)
	}
}

func TestStartReportsAnAppThatClosesWithoutAWindow(t *testing.T) {
	old := statusPoll
	statusPoll = 10 * time.Millisecond
	defer func() { statusPoll = old }()
	start, _ := fakeHelper(t, fakeOpts{noWin: true})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
	_, err := m.Start(context.Background(), "vm1-crash", `C:\a.exe`, "")
	if err == nil || !strings.Contains(err.Error(), "0x9") {
		t.Fatalf("err = %v, want the exit code", err)
	}
}

func TestStartRefusesTargetsThatCouldBreakTheLineProtocol(t *testing.T) {
	m := NewManager(func(context.Context, string) (*exec.Cmd, error) {
		t.Fatal("must not start a helper for an invalid target")
		return nil, nil
	})
	for _, target := range []string{"", "a\nb", "a\tb", "a\rb"} {
		if _, err := m.Start(context.Background(), "x", target, ""); err == nil {
			t.Errorf("target %q accepted", target)
		}
	}
	if _, err := m.Start(context.Background(), "x", "a.exe", "b\nquit"); err == nil {
		t.Error("args with a newline accepted: they could inject a protocol command")
	}
}

func TestStartTwiceReusesTheRunningSession(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})
	a, err := m.Start(context.Background(), "same", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Start(context.Background(), "same", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("a second start of the same app made a second session")
	}
}

func TestTwoStartsAtOnceLaunchTheAppOnce(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})

	var wg sync.WaitGroup
	got := make([]*Session, 6)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := m.Start(context.Background(), "race", `C:\a.exe`, "")
			if err != nil {
				t.Error(err)
			}
			got[i] = s
		}(i)
	}
	wg.Wait()
	for _, s := range got {
		if s != got[0] {
			t.Fatal("concurrent starts of one app made more than one session: the app would open twice on one desktop")
		}
	}
	launches := 0
	for _, l := range received(t) {
		if strings.HasPrefix(l, "launch") {
			launches++
		}
	}
	if launches != 1 {
		t.Errorf("the app was launched %d times, want 1", launches)
	}
}

func TestTheNextAppFindsAHelperAlreadyWaiting(t *testing.T) {
	m, starts := fakeManager(t, fakeOpts{})

	m.Warm()
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.Lock()
		ready := m.spare != nil
		m.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no warm helper appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Start(context.Background(), "warm", `C:\a.exe`, ""); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(starts); n < 1 || n > 2 {
		// One for the warm helper; a replacement for the next app may already be starting, but the app itself did not wait for one.
		t.Errorf("%d helpers started", n)
	}
	// After the app took it, another is started for whoever comes next.
	deadline = time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(starts) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no replacement helper was started for the next app")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAnUnclaimedHelperIsLetGo(t *testing.T) {
	old := spareTTL
	spareTTL = 100 * time.Millisecond
	defer func() { spareTTL = old }()
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

	h, err := m.helper(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.keepWarm(h)
	deadline := time.Now().Add(3 * time.Second)
	for h.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("an unused helper was kept for ever: it holds memory on the host for a feature nobody is using")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIdleSessionEndsItself(t *testing.T) {
	oldIdle, oldPrelaunch := idleStop, prelaunchIdleStop
	idleStop = 150 * time.Millisecond
	prelaunchIdleStop = 150 * time.Millisecond
	defer func() { idleStop, prelaunchIdleStop = oldIdle, oldPrelaunch }()

	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
	s, err := m.Start(context.Background(), "idle", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for s.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("an unwatched session was left running: its app would stay open, invisibly, on the host")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := m.Get("idle"); ok {
		t.Error("ended session still listed")
	}
}

// TestPrelaunchedSessionOutlivesIdleStopUntilWatched is what hostAppStarter (started at VM creation or an app copy,
// before anyone could possibly have opened the viewer yet) depends on: a session nobody has ever watched must not be
// judged by the same idleStop that ends a session someone opened and then walked away from - it would defeat the
// entire point of starting the app ahead of time. It still ends on its own past prelaunchIdleStop, so a desktop
// nobody ever visits does not run its host apps forever.
func TestPrelaunchedSessionOutlivesIdleStopUntilWatched(t *testing.T) {
	oldIdle, oldPrelaunch := idleStop, prelaunchIdleStop
	idleStop = 150 * time.Millisecond
	prelaunchIdleStop = 900 * time.Millisecond
	defer func() { idleStop, prelaunchIdleStop = oldIdle, oldPrelaunch }()

	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
	s, err := m.Start(context.Background(), "prelaunch", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}

	// Well past idleStop, with nobody ever having watched it.
	time.Sleep(500 * time.Millisecond)
	if !s.Alive() {
		t.Fatal("a never-watched, ahead-of-time session was ended at idleStop: starting an app early bought nothing")
	}

	deadline := time.Now().Add(6 * time.Second)
	for s.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("a never-watched session was left running forever: it must still end past prelaunchIdleStop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAWatchedSessionIsNotEndedJustBecauseItsPictureIsStill(t *testing.T) {
	old := idleStop
	idleStop = 200 * time.Millisecond
	defer func() { idleStop = old }()

	_, s := startFake(t, "watched")
	v := s.Watch()

	// Well past idleStop, with a viewer connected and nothing changing on screen.
	time.Sleep(1800 * time.Millisecond)
	if !s.Alive() {
		t.Fatal("a session with a viewer connected was ended for being idle: the app closes while someone is looking at it")
	}

	// Once the viewer goes away it is unwatched like any other and ends itself.
	v.Close()
	deadline := time.Now().Add(6 * time.Second)
	for s.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("a session nobody watches any more was never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheHelpersCaptureFollowsTheViewers(t *testing.T) {
	_, s := startFake(t, "viewers")
	v1 := s.Watch()
	v2 := s.Watch()
	v1.Close()
	v2.Close()
	time.Sleep(100 * time.Millisecond)
	// Only the session's own helper's lines (the one that launched the app): the replacement warm helper's readiness
	// ping ("viewers 0") went to the same log and, landing between the two viewers, read as the count falling to 0
	// with a viewer watching (CI, 2026-11-08: 1,0,2,1,0).
	time.Sleep(50 * time.Millisecond)
	out, _ := exec.Command("cat", getenv("FAKE_LOG")+".byhelper").Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	session := ""
	for _, l := range lines {
		if pid, rest, ok := strings.Cut(l, " "); ok && strings.HasPrefix(rest, "launch") {
			session = pid
		}
	}
	var counts []string
	for _, l := range lines {
		if pid, rest, ok := strings.Cut(l, " "); ok && pid == session && strings.HasPrefix(rest, "viewers ") {
			counts = append(counts, strings.TrimPrefix(rest, "viewers "))
		}
	}
	// The start holds the capture on while the window appears and lets go; then two viewers come and both go.
	if got := "," + strings.Join(counts, ",") + ","; !strings.Contains(got, ",1,2,1,0,") {
		t.Errorf("viewer counts sent to the helper = %s, want them to rise with each viewer and fall to 0 so the helper stops capturing", got)
	}
}

func TestAnAppThatClosesItselfEndsTheSessionAndSaysSo(t *testing.T) {
	_, s := startFake(t, "closes")
	if _, err := s.Do("close"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.AppClosed():
	case <-time.After(3 * time.Second):
		t.Fatal("the app closing was never noticed")
	}
	select {
	case <-s.Done():
	case <-time.After(8 * time.Second):
		t.Fatal("the helper was left running after its app closed")
	}
}

func TestStopAllEndsEverySessionAndTheWarmHelper(t *testing.T) {
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	s, err := m.Start(context.Background(), "a", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	spare, err := m.helper(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.keepWarm(spare)
	m.StopAll()
	m.waitIdle()
	if s.Alive() || spare.Alive() {
		t.Fatalf("after StopAll the session is alive=%v and the warm helper alive=%v", s.Alive(), spare.Alive())
	}
}

func TestScriptIsEmbedded(t *testing.T) {
	if !strings.Contains(string(Script), "CreateDesktop") || strings.Contains(string(Script), "HttpListener") {
		t.Fatal("bridge.ps1 must create a private desktop and must not open a network listener")
	}
	if !strings.Contains(string(Script), "Initialize-PrivateBrowserProfile") ||
		!strings.Contains(string(Script), `Google\Chrome\User Data`) ||
		!strings.Contains(string(Script), ".decurix-host-profile-imported") {
		t.Fatal("bridge.ps1 lost the private Chrome profile setup")
	}
	// The person's own browser profile (cookies, saved passwords, the key they are encrypted with) is never copied
	// into a browser a worker drives unless this computer's owner opted in (review 2026-11-02).
	src := string(Script)
	i := strings.Index(src, "function Initialize-PrivateBrowserProfile(")
	head := src[i:min(len(src), i+1500)]
	optIn := strings.Index(head, "if ($env:DECURIX_IMPORT_HOST_BROWSER_PROFILE -ne '1') { return }")
	copyAt := strings.Index(head, "robocopy")
	if optIn < 0 || (copyAt >= 0 && copyAt < optIn) {
		t.Fatal("bridge.ps1 must not import the host's browser profile without the explicit opt-in")
	}
}

// TestSetValueLeavesADialogsComboBoxToTypedKeys pins a live finding: Word's Save As ignored a path set through UI Automation
// on its "File name:" box (it saved "Doc1" in Documents) and kept one typed with keys.
func TestSetValueLeavesADialogsComboBoxToTypedKeys(t *testing.T) {
	src := string(Script)
	i := strings.Index(src, "public static string SetValueAt(")
	if i < 0 {
		t.Fatal("SetValueAt is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "static void RunOnDesktop"); j > 0 {
		body = body[:j]
	}
	for _, want := range []string{`"#32770"`, "GW_OWNER", "ControlType.ComboBox", "takes typed keys"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SetValueAt no longer leaves a dialog's combo box to typed keys: missing %s", want)
		}
	}
}

func TestModifiersAreValidatedBeforeTheyReachTheHost(t *testing.T) {
	_, s := startFake(t, "mods")
	for _, bad := range []string{"x", "cc", "c s", "c\n", "ctrl"} {
		if s.Key(65, bad) == nil || s.Mouse("down", 1, 1, false, bad) == nil {
			t.Errorf("modifiers %q were accepted: they are written into a protocol line", bad)
		}
	}
	for _, good := range []string{"", "c", "s", "a", "cs", "csa", "ac"} {
		if err := s.Key(65, good); err != nil {
			t.Errorf("modifiers %q refused: %v", good, err)
		}
	}
	if s.Key(0, "") == nil || s.Key(256, "") == nil {
		t.Error("an out-of-range key was accepted")
	}
}

func TestPasteTypesTextLineByLineAndNeverNeedsTheClipboard(t *testing.T) {
	_, s := startFake(t, "paste")
	if err := s.Paste("first line\r\nsecond\n\nfourth"); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.Join(received(t), "|"), "|")
	var typed []string
	for _, l := range got {
		if strings.HasPrefix(l, "type ") || l == "newline" {
			typed = append(typed, l)
		}
	}
	want := []string{"type first line", "newline", "type second", "newline", "newline", "type fourth"}
	if strings.Join(typed, "|") != strings.Join(want, "|") {
		t.Fatalf("pasting sent %q, want %q", typed, want)
	}
	if err := s.Paste(strings.Repeat("x", maxPaste+1)); err == nil {
		t.Fatal("an enormous paste was accepted")
	}
}

func TestCopyOnlyPressesCopyOrCutAndReadsWhatTheAppCopied(t *testing.T) {
	_, s := startFake(t, "copy")
	if text, err := s.Copy(67); err != nil || text != "hello world" {
		t.Fatalf("Copy(67) = %q, %v", text, err)
	}
	if text, err := s.Copy(88); err != nil || text != "" {
		t.Fatalf("a copy that changed nothing = %q, %v, want empty", text, err)
	}
	for _, vk := range []int{0, 65, 86, 255} {
		if _, err := s.Copy(vk); err == nil {
			t.Errorf("Copy(%d) was allowed: only Ctrl+C and Ctrl+X may read the person's clipboard", vk)
		}
	}
}

func TestCheckReportsWhichProgramsCanRunWithoutStartingAny(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})
	paint := Target{Target: `C:\Windows\mspaint.exe`}
	calc := Target{Target: `shell:AppsFolder\uwp!App`}
	got, err := m.Check(context.Background(), []Target{paint, calc})
	if err != nil {
		t.Fatal(err)
	}
	if got[paint] != "" {
		t.Errorf("paint: %q, want it to be runnable", got[paint])
	}
	if !strings.Contains(got[calc], "UWP") {
		t.Errorf("calculator: %q, want the helper's reason", got[calc])
	}
	for _, line := range received(t) {
		if strings.HasPrefix(line, "launch") {
			t.Errorf("a check started an app: %q", line)
		}
	}
}

func TestACheckLeavesItsHelperWarmForTheNextLaunch(t *testing.T) {
	m, starts := fakeManager(t, fakeOpts{})
	if _, err := m.Check(context.Background(), []Target{{Target: "a.exe"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), "after-check", `C:\a.exe`, ""); err != nil {
		t.Fatal(err)
	}
	// One helper did the check and then the launch; a second may be starting for whoever comes next, but never a third.
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(starts); n > 2 {
		t.Errorf("%d helpers started: the launch did not reuse the one that checked", n)
	}
	launched := false
	for _, l := range received(t) {
		if strings.HasPrefix(l, "check\t") {
			launched = false
		}
		if strings.HasPrefix(l, "launch\t") {
			launched = true
		}
	}
	if !launched {
		t.Error("nothing was launched")
	}
}

func TestCheckAnswersTargetsTheProtocolCannotCarryItself(t *testing.T) {
	bad := Target{Target: "a\tb"}
	m := NewManager(func(context.Context, string) (*exec.Cmd, error) {
		t.Fatal("the helper was started for nothing but invalid targets")
		return nil, nil
	})
	got, err := m.Check(context.Background(), []Target{bad, {Target: "  "}, {Target: "x", Args: "a\nb"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[bad] == "" {
		t.Errorf("got %+v, want all three refused", got)
	}
}

func TestCheckKeepsWhatWasAnsweredWhenTheHelperDies(t *testing.T) {
	requireBash(t)
	var starts int32
	m := NewManager(func(context.Context, string) (*exec.Cmd, error) {
		// The first answers the readiness ping and one check, then exits; every later one dies after the ping.
		if atomic.AddInt32(&starts, 1) == 1 {
			return exec.Command("bash", "-c", `read -r l; echo OK; read -r l; echo OK`), nil
		}
		return exec.Command("bash", "-c", `read -r l; echo OK`), nil
	})
	defer m.StopAll()
	first, second := Target{Target: "a.exe"}, Target{Target: "b.exe"}
	got, err := m.Check(context.Background(), []Target{first, second})
	if err == nil {
		t.Fatal("expected the helpers' deaths to be reported")
	}
	if v, ok := got[first]; !ok || v != "" {
		t.Errorf("first = %q, %v; want its answer kept", v, ok)
	}
	if _, ok := got[second]; ok {
		t.Error("second was never answered but has a verdict")
	}
	if n := atomic.LoadInt32(&starts); n != int32(1+checkHelperTries) {
		t.Errorf("helpers started = %d, want the first plus %d that answered nothing", n, checkHelperTries)
	}
}

func TestCheckAsksTheRestOnANewHelperWhenOneDiesPartway(t *testing.T) {
	start, starts := fakeHelper(t, fakeOpts{})
	var n int32
	m := NewManager(func(ctx context.Context, name string) (*exec.Cmd, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			// Answers the readiness ping and two checks, then exits - the boot-time hiccup.
			return exec.Command("bash", "-c", `read -r l; echo OK; read -r l; echo OK; read -r l; echo OK`), nil
		}
		return start(ctx, name)
	})
	defer m.StopAll()
	var targets []Target
	for _, s := range []string{"a.exe", "b.exe", "c.exe", "d.exe", "uwp.exe"} {
		targets = append(targets, Target{Target: s})
	}
	got, err := m.Check(context.Background(), targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(targets) {
		t.Fatalf("got %d verdicts, want all %d: %+v", len(got), len(targets), got)
	}
	if got[Target{Target: "c.exe"}] != "" || got[Target{Target: "uwp.exe"}] == "" {
		t.Errorf("verdicts = %+v; want c runnable and the UWP app refused", got)
	}
	if atomic.LoadInt32(starts) != 1 {
		t.Errorf("replacement helpers = %d, want 1", atomic.LoadInt32(starts))
	}
}

func launches(t *testing.T) (full, resolved int) {
	t.Helper()
	for _, l := range received(t) {
		switch {
		case strings.HasPrefix(l, "launchr\t"):
			resolved++
		case strings.HasPrefix(l, "launch\t"):
			full++
		}
	}
	return
}

func TestALaunchAfterACheckStartsWhatWasResolvedWithoutLookingItUpAgain(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})
	if _, err := m.Check(context.Background(), []Target{{Target: `shell:AppsFolder\Chrome`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), "resolved", `shell:AppsFolder\Chrome`, ""); err != nil {
		t.Fatal(err)
	}
	if full, resolved := launches(t); full != 0 || resolved != 1 {
		t.Fatalf("full launches %d, resolved launches %d: a checked program must be started from what the check found", full, resolved)
	}
	// An app nobody checked is still launchable: the helper looks it up itself.
	if _, err := m.Start(context.Background(), "unchecked", `C:\other.exe`, ""); err != nil {
		t.Fatal(err)
	}
	if full, _ := launches(t); full != 1 {
		t.Fatalf("full launches %d, want 1 for the program that was never checked", full)
	}
}

func TestWhatACheckResolvedSurvivesTheHelperThatDidIt(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})
	m.Check(context.Background(), []Target{{Target: "a.exe"}})
	// Throw away every helper, as happens when one is used up by an app or let go: the knowledge must not go with it.
	m.mu.Lock()
	spare := m.spare
	m.spare = nil
	m.mu.Unlock()
	spare.Stop()
	if _, err := m.Start(context.Background(), "fresh", "a.exe", ""); err != nil {
		t.Fatal(err)
	}
	if full, resolved := launches(t); full != 0 || resolved != 1 {
		t.Fatalf("full %d resolved %d: a brand-new helper made the launch look the program up again", full, resolved)
	}
}

func TestAProgramThatHasMovedIsLookedUpAgainAndNotTrustedTwice(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{moved: true})
	m.Check(context.Background(), []Target{{Target: "a.exe"}})
	if _, err := m.Start(context.Background(), "moved", "a.exe", ""); err != nil {
		t.Fatalf("a program that moved must still start, found again by the helper: %v", err)
	}
	if full, resolved := launches(t); resolved != 1 || full != 1 {
		t.Fatalf("resolved %d, full %d: want the stale answer tried once and then a full lookup", resolved, full)
	}
	m.mu.Lock()
	_, still := m.resolved[Target{Target: "a.exe"}]
	m.mu.Unlock()
	if still {
		t.Error("a stale resolution was kept")
	}
}

func TestAskingForAWarmHelperKeepsTheOneThatIsWaiting(t *testing.T) {
	old := spareTTL
	spareTTL = 300 * time.Millisecond
	defer func() { spareTTL = old }()
	start, starts := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

	m.Warm()
	waitUntil(t, "a warm helper", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.spare != nil })
	h := m.spare
	// Well past the TTL, but somebody keeps showing interest: the same helper is still there, not started again.
	for i := 0; i < 8; i++ {
		time.Sleep(100 * time.Millisecond)
		m.Warm()
	}
	if !h.Alive() || atomic.LoadInt32(starts) != 1 {
		t.Fatalf("alive=%v starts=%d: a helper that people keep asking for was let go or replaced", h.Alive(), atomic.LoadInt32(starts))
	}
	// When interest stops it is let go, as before.
	waitUntil(t, "the unwanted helper to be let go", func() bool { return !h.Alive() })
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFor polls cond for up to 6s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAKeptAppIsRunningBeforeAnybodyOpensItAndComesBackAfterItCloses is what makes opening a copied Windows app
// immediate: it is already running when a VM's viewer asks for it, and closing it (from inside the app, or by closing
// the viewer) starts a fresh one for next time.
func TestAKeptAppIsRunningBeforeAnybodyOpensItAndComesBackAfterItCloses(t *testing.T) {
	old := keepRelaunch
	keepRelaunch = 50 * time.Millisecond
	defer func() { keepRelaunch = old }()

	start, starts := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

	m.Keep("vm1", "vm1-app", `C:\a.exe`, "")
	waitFor(t, "a kept app was not started ahead of time", func() bool { return m.Alive("vm1-app") })
	first, _ := m.Get("vm1-app")

	// The person closes the app from inside it.
	if _, err := first.Do("close"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a kept app that was closed was not started again", func() bool {
		s, ok := m.Get("vm1-app")
		return ok && s != first
	})

	// The person closes the window showing it.
	second, _ := m.Get("vm1-app")
	m.Stop("vm1-app")
	waitFor(t, "a kept app whose viewer was closed was not started again", func() bool {
		s, ok := m.Get("vm1-app")
		return ok && s != second
	})

	// Opening it joins the one waiting: no launch while somebody watches.
	before := atomic.LoadInt32(starts)
	waiting, _ := m.Get("vm1-app")
	s, err := m.Start(context.Background(), "vm1-app", `C:\a.exe`, "")
	if err != nil || s != waiting {
		t.Fatalf("opening a kept app started another one (%v)", err)
	}
	if atomic.LoadInt32(starts) != before {
		t.Error("opening a kept app started a helper")
	}
}

func TestAForgottenAppIsEndedAndNotStartedAgain(t *testing.T) {
	old := keepRelaunch
	keepRelaunch = 50 * time.Millisecond
	defer func() { keepRelaunch = old }()

	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

	m.Keep("vm1", "vm1-a", `C:\a.exe`, "")
	m.Keep("vm2", "vm2-a", `C:\a.exe`, "")
	waitFor(t, "kept apps were not started", func() bool { return m.Alive("vm1-a") && m.Alive("vm2-a") })
	s, _ := m.Get("vm1-a")

	m.ForgetDesktop("vm1")
	if s.Alive() {
		t.Fatal("the app of a deleted VM was left running on the host")
	}
	time.Sleep(300 * time.Millisecond)
	if m.Alive("vm1-a") {
		t.Error("the app of a deleted VM was started again")
	}
	if !m.Alive("vm2-a") {
		t.Error("forgetting one VM's apps ended another's")
	}
}

// TestAKeptAppNobodyHasOpenedIsNotEndedForBeingUnwatched: ending it would only start it again, and the whole point is that
// it is there when somebody first opens it, however long that takes.
func TestAKeptAppNobodyHasOpenedIsNotEndedForBeingUnwatched(t *testing.T) {
	oldIdle, oldPrelaunch := idleStop, prelaunchIdleStop
	idleStop, prelaunchIdleStop = 100*time.Millisecond, 100*time.Millisecond
	defer func() { idleStop, prelaunchIdleStop = oldIdle, oldPrelaunch }()

	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

	m.Keep("vm1", "vm1-a", `C:\a.exe`, "")
	waitFor(t, "a kept app was not started", func() bool { return m.Alive("vm1-a") })
	s, _ := m.Get("vm1-a")
	time.Sleep(1500 * time.Millisecond)
	if !s.Alive() {
		t.Fatal("a kept app nobody had opened yet was ended for being unwatched")
	}
}

// TestOpeningAnAppThatIsClosingWaitsForTheNextOne: the closing one and a new one would share a private desktop, and the
// closing one's shutdown ends every window on it.
func TestOpeningAnAppThatIsClosingWaitsForTheNextOne(t *testing.T) {
	m, first := startFake(t, "closing")
	if _, err := first.Do("close"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the app never said it had closed", first.ending)

	s, err := m.Start(context.Background(), "closing", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	if s == first {
		t.Fatal("opening an app that was closing joined the closing one")
	}
	if first.Alive() {
		t.Error("a new app was started on the private desktop of one still closing")
	}
}

// TestAWebAppsLauncherIsKnownByItsLocalPage: a check that finds a target to be a web app's launcher - a browser opening
// a page on this computer - says so (the seventh field), and LocalPage gives that page; a plain program, a browser with
// no launcher behind it and a page on another computer give none.
func TestAWebAppsLauncherIsKnownByItsLocalPage(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})
	web := Target{Target: `C:\webapp.exe`}
	plain := Target{Target: `C:\Windows\mspaint.exe`}
	if _, err := m.Check(context.Background(), []Target{web, plain}); err != nil {
		t.Fatal(err)
	}
	if got := m.LocalPage(web.Target, web.Args); got != "http://localhost:8000" {
		t.Errorf("web app launcher: LocalPage %q, want its page", got)
	}
	if got := m.LocalPage(plain.Target, plain.Args); got != "" {
		t.Errorf("plain program: LocalPage %q, want none", got)
	}
}

// TestLocalPageIsOnlyALaunchersPageOnThisComputer reads what a check resolved, with no helper.
func TestLocalPageIsOnlyALaunchersPageOnThisComputer(t *testing.T) {
	m := NewManager(nil)
	cases := []struct {
		name     string
		r        resolvedProgram
		wantPage string
	}{
		{"launcher, localhost", resolvedProgram{exe: "msedge.exe", args: "--app=http://localhost:8000/app", launcher: `C:\portal.exe`}, "http://localhost:8000/app"},
		{"launcher, 127.0.0.1", resolvedProgram{exe: "chrome.exe", args: `--app="http://127.0.0.1:3000" --x`, launcher: `C:\a.exe`}, "http://127.0.0.1:3000"},
		{"launcher, another computer", resolvedProgram{exe: "msedge.exe", args: "--app=https://portal.example.com", launcher: `C:\a.exe`}, ""},
		{"a browser nobody's launcher opened", resolvedProgram{exe: "msedge.exe", args: "--app=http://localhost:8000"}, ""},
		{"launcher, no page", resolvedProgram{exe: "msedge.exe", args: "--new-window", launcher: `C:\a.exe`}, ""},
		{"launcher, not a web address", resolvedProgram{exe: "msedge.exe", args: "--app=file:///C:/x.html", launcher: `C:\a.exe`}, ""},
	}
	for i, c := range cases {
		target := fmt.Sprintf(`C:\t%d.exe`, i)
		m.resolved[Target{Target: target}] = c.r
		if got := m.LocalPage(target, ""); got != c.wantPage {
			t.Errorf("%s: LocalPage %q, want %q", c.name, got, c.wantPage)
		}
	}
	if got := m.LocalPage(`C:\never-checked.exe`, ""); got != "" {
		t.Errorf("a target no check resolved: %q, want none", got)
	}
}

// TestALaunchOfAWebAppStillNamesItsLauncher: launchr carries the launcher, so the helper starts the app's server before
// the page, as a launch it looked up itself would.
func TestALaunchOfAWebAppStillNamesItsLauncher(t *testing.T) {
	r := resolvedProgram{exe: `C:\Edge\msedge.exe`, args: "--app=http://localhost:8000", own: "1", base: "msedge", launcher: `C:\portal.exe`}
	if got, want := r.line(), "launchr\tC:\\Edge\\msedge.exe\t--app=http://localhost:8000\t\t1\tmsedge\tC:\\portal.exe"; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
}

func TestTypedLineBreaksAreLineBreaksAndTheTypingModeIsSentOnce(t *testing.T) {
	_, s := startFake(t, "lines")
	if err := s.Type("Dear Ms Menon,\n\nWe are sorry"); err != nil {
		t.Fatal(err)
	}
	s.SetKeyedText(true)
	s.SetKeyedText(true)
	s.SetKeyedText(false)
	var sent []string
	for _, l := range received(t) {
		if strings.HasPrefix(l, "type ") || l == "newline" || strings.HasPrefix(l, "keyed ") {
			sent = append(sent, l)
		}
	}
	want := []string{"type Dear Ms Menon,", "newline", "newline", "type We are sorry", "keyed 1", "keyed 0"}
	if strings.Join(sent, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q, want %q", sent, want)
	}
}

// A session's end ends what runs its helper on the host, however it ended. Found live (2026-10-20): the helper ran in
// a container started by a docker client; ending the session killed the client, the container kept the helper's stdin
// open, and a dozen helpers were left running in three minutes. The command's Cancel is what removes the container
// (desktop.HopCommand), so a session must call it.
func TestEndingASessionEndsWhatRunsItsHelper(t *testing.T) {
	start, _ := fakeHelper(t, fakeOpts{})
	var mu sync.Mutex
	cancels := map[string]int{}
	counting := func(ctx context.Context, name string) (*exec.Cmd, error) {
		plain, err := start(ctx, name)
		if err != nil {
			return nil, err
		}
		// A Cancel needs a command made with a context, as every host hop is (desktop.HopCommand).
		cmd := exec.CommandContext(context.Background(), plain.Args[0], plain.Args[1:]...)
		cmd.Env = plain.Env
		cmd.Cancel = func() error {
			mu.Lock()
			cancels[name]++
			mu.Unlock()
			if cmd.Process != nil {
				return cmd.Process.Kill()
			}
			return nil
		}
		return cmd, nil
	}
	ended := func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return cancels[name]
	}

	m := NewManager(counting)
	s, err := m.Start(context.Background(), "vm-app", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for ended("decurix_helper_1") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ended("decurix_helper_1") == 0 {
		t.Fatalf("a stopped session did not end what ran its helper: %v", cancels)
	}
	m.StopAll()
	m.waitIdle()
}

// A helper whose client dies by itself (the stream broke) is ended the same way, though nothing called Stop.
func TestAHelperWhoseClientExitsIsEnded(t *testing.T) {
	requireBash(t)
	var calls int32
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "read l; exit 0")
	cmd.Cancel = func() error { atomic.AddInt32(&calls, 1); return nil }
	s, err := newSession("x", cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.stdin.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the helper did not end")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("Cancel called %d times after the client exited", calls)
	}
}

// TestAKeptAppThatClosesStraightAwayIsGivenUpOnUntilKeptAgain: found live (2026-10-20), a Store app that cannot run on a
// private desktop was relaunched every two minutes, for hours, for a stopped VM. After keepGiveUpAfter launches that
// closed straight away it is not tried again, and whoever keeps the list is told why; keeping it again tries again.
func TestAKeptAppThatClosesStraightAwayIsGivenUpOnUntilKeptAgain(t *testing.T) {
	requireBash(t)
	oldMin, oldMax := keepRetryMin, keepRetryMax
	keepRetryMin, keepRetryMax = 10*time.Millisecond, 20*time.Millisecond
	defer func() { keepRetryMin, keepRetryMax = oldMin, oldMax }()

	start, starts := fakeHelper(t, fakeOpts{noWin: true})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
	var gaveUp []string
	var mu sync.Mutex
	m.OnGiveUp = func(desktop, id, reason string) {
		mu.Lock()
		defer mu.Unlock()
		gaveUp = append(gaveUp, desktop+"/"+id+": "+reason)
	}

	m.Keep("vm1", "vm1-notepad", `C:\notepad.exe`, "")
	waitFor(t, "the app was not given up on", func() bool { mu.Lock(); defer mu.Unlock(); return len(gaveUp) == 1 })
	if !strings.Contains(gaveUp[0], "vm1/vm1-notepad: It cannot run here") || !strings.Contains(gaveUp[0], "closed straight away") {
		t.Errorf("reason = %q", gaveUp[0])
	}
	launched := atomic.LoadInt32(starts)
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(starts); n != launched {
		t.Errorf("a given-up app was launched again (%d helpers -> %d)", launched, n)
	}
	if got := m.KeptFor("vm1"); len(got) != 1 || got[0] != "vm1-notepad" {
		t.Errorf("KeptFor = %v: the app stays on the list, given up on", got)
	}

	// Kept again (its desktop started again, or the app copied again): tried again.
	m.Keep("vm1", "vm1-notepad", `C:\notepad.exe`, "")
	waitFor(t, "a given-up app kept again was not tried again", func() bool { return atomic.LoadInt32(starts) > launched })
}

// TestStopEndsWhatTheHelperStartedOnTheHost: Stop hands the manager's Reap the helper's own host process id and the
// app it launched - found live (2026-10-20), a stopped helper and its WINWORD.EXE kept running on the person's PC.
func TestStopEndsWhatTheHelperStartedOnTheHost(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{launch: "OK pid=42"})
	var mu sync.Mutex
	var reaped [][2]int
	m.Reap = func(helper, app int) {
		mu.Lock()
		reaped = append(reaped, [2]int{helper, app})
		mu.Unlock()
	}
	if _, err := m.Start(context.Background(), "vm1-word", `C:\Word.exe`, ""); err != nil {
		t.Fatal(err)
	}
	if got := m.HostPIDs(); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("HostPIDs = %v, want [4242]", got)
	}
	m.Stop("vm1-word")
	mu.Lock()
	defer mu.Unlock()
	if len(reaped) != 1 || reaped[0] != [2]int{4242, 42} {
		t.Fatalf("reaped %v, want the helper 4242 and its app 42", reaped)
	}
}

func TestParsePID(t *testing.T) {
	for in, want := range map[string]int{"OK 4242": 4242, "OK pid=42 sandbox=S-1": 42, "OK": 0, "ERR unknown command": 0,
		"OK pid=x": 0, "OK -3": 0} {
		if got := parsePID(in); got != want {
			t.Errorf("parsePID(%q) = %d, want %d", in, got, want)
		}
	}
}
