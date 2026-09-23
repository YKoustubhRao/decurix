package winhost

import (
	"context"
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
	script := `
dolaunch() { echo "` + o.launch + `"; if [ "` + noWin + `" = 0 ] && [[ "` + o.launch + `" == OK* ]]; then send 1 0 0 20 10 ABC; fi; }
send() { printf 'FRAME 20 10 %s %s %s %s %s %s\n%s' "$2" "$3" "$4" "$5" "$1" "${#6}" "$6"; }
while IFS= read -r l; do
  printf '%s\n' "$l" >> "$FAKE_LOG"
  case "$l" in
    launchr*) if [ "` + moved + `" = 1 ]; then echo "ERR Not found on this computer: gone"; else dolaunch; fi;;
    launch*)  dolaunch;;
    keyframe) echo OK; send 1 0 0 20 10 ABC;;
    status)   if [ "` + noWin + `" = 1 ]; then echo "STATUS exited 0x9"; else echo "STATUS running"; fi;;
    emit*)    set -- $l; echo OK; send "$2" "$3" "$4" "$5" "$6" "$7";;
    close)    echo OK; echo "EVENT closed";;
    "copy 67") echo "CLIP aGVsbG8gd29ybGQ=";;
    "copy 88") echo "CLIP";;
    check*uwp*) echo "ERR This is a Universal Windows (UWP) app";;
    check*)   IFS=$'\t' read -r _ t a <<< "$l"; printf 'OK\t%s\t%s\t\t0\tx\n' "$t" "$a";;
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

func startFake(t *testing.T, id string) (*Manager, *Session) {
	t.Helper()
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	t.Cleanup(m.waitIdle)
	t.Cleanup(m.StopAll)
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
	start, _ := fakeHelper(t, fakeOpts{launch: "OK pid=42"})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

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

func TestStartReturnsTheHelpersReasonWhenTheLaunchIsRefused(t *testing.T) {
	start, starts := fakeHelper(t, fakeOpts{launch: "ERR This is a Microsoft Store app."})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
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
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
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
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

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
	start, starts := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()

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
	old := idleStop
	idleStop = 150 * time.Millisecond
	defer func() { idleStop = old }()

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
	var counts []string
	for _, l := range received(t) {
		if strings.HasPrefix(l, "viewers ") {
			counts = append(counts, strings.TrimPrefix(l, "viewers "))
		}
	}
	// The start holds the capture on while the window appears and lets go; then two viewers come and both go. The
	// replacement warm helper's own readiness ping ("viewers 0") lands in the same log, so look for the sequence, not the end.
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
		t.Fatal("bridge.ps1 must seed a private Chrome profile from the host user's signed-in profile")
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
		if strings.HasPrefix(l, "type ") || l == "key 13" {
			typed = append(typed, l)
		}
	}
	want := []string{"type first line", "key 13", "type second", "key 13", "key 13", "type fourth"}
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
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
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
	start, starts := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
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
	m := NewManager(func(context.Context, string) (*exec.Cmd, error) {
		// Answers the readiness ping and one check, then exits.
		return exec.Command("bash", "-c", `read -r l; echo OK; read -r l; echo OK`), nil
	})
	defer m.StopAll()
	first, second := Target{Target: "a.exe"}, Target{Target: "b.exe"}
	got, err := m.Check(context.Background(), []Target{first, second})
	if err == nil {
		t.Fatal("expected the helper's death to be reported")
	}
	if v, ok := got[first]; !ok || v != "" {
		t.Errorf("first = %q, %v; want its answer kept", v, ok)
	}
	if _, ok := got[second]; ok {
		t.Error("second was never answered but has a verdict")
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
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
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
	start, _ := fakeHelper(t, fakeOpts{})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
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
	start, _ := fakeHelper(t, fakeOpts{moved: true})
	m := NewManager(start)
	defer m.waitIdle()
	defer m.StopAll()
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
