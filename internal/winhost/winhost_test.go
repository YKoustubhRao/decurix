package winhost

import (
	"context"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHelper stands in for bridge.ps1: the same line protocol, answered by a bash loop. Every line it receives is
// appended to the file named by log, so a test can see exactly what reached the host.
func fakeHelper(t *testing.T, launch string, frames []string) Starter {
	t.Helper()
	quoted := make([]string, len(frames))
	for i, f := range frames {
		quoted[i] = "'" + f + "'"
	}
	log := t.TempDir() + "/received.log"
	t.Setenv("FAKE_LOG", log)
	script := `
n=0
frames=(` + strings.Join(quoted, " ") + `)
while IFS= read -r l; do
  printf '%s\n' "$l" >> "$FAKE_LOG"
  case "$l" in
    launch*) echo "` + launch + `";;
    frame)   echo "${frames[$n]}"; if [ $n -lt $((${#frames[@]}-1)) ]; then n=$((n+1)); fi;;
    status)  echo "STATUS exited 0x9";;
    quit)    exit 0;;
    *)       echo "OK";;
  esac
done`
	return func(ctx context.Context, name string) (*exec.Cmd, error) {
		return exec.Command("bash", "-c", script), nil
	}
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

func TestStartWaitsForFirstFrameAndForwardsInputVerbatim(t *testing.T) {
	m := NewManager(fakeHelper(t, "OK pid=42", []string{"FRAME 4 3 QUJD"}))
	defer m.StopAll()

	s, err := m.Start(context.Background(), "vm1-app", `C:\Program Files\App\app.exe`, "--flag")
	if err != nil {
		t.Fatal(err)
	}
	f, ok := s.Latest()
	if !ok || f.W != 4 || f.H != 3 || string(f.JPEG) != "ABC" {
		t.Fatalf("first frame = %+v ok=%v", f, ok)
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
	want := "launch\tC:\\Program Files\\App\\app.exe\t--flag"
	for _, w := range []string{want, "down 10 20", "up 10 20 right +cs", "type hello world", "key 13 c", "wheel 1 2 -3"} {
		if !strings.Contains(got, w) {
			t.Errorf("helper never received %q; got %q", w, got)
		}
	}
}

func TestStartReturnsTheHelpersReasonWhenTheLaunchIsRefused(t *testing.T) {
	m := NewManager(fakeHelper(t, "ERR This is a Microsoft Store app.", []string{"NOFRAME"}))
	_, err := m.Start(context.Background(), "vm1-calc", `shell:AppsFolder\X`, "")
	if err == nil || !strings.Contains(err.Error(), "Microsoft Store app") {
		t.Fatalf("err = %v, want the helper's own reason", err)
	}
}

func TestStartReportsAnAppThatClosesWithoutAWindow(t *testing.T) {
	m := NewManager(fakeHelper(t, "OK pid=7", []string{"NOFRAME"}))
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
	var starts int32
	inner := fakeHelper(t, "OK pid=1", []string{"FRAME 1 1 QQ=="})
	m := NewManager(func(ctx context.Context, name string) (*exec.Cmd, error) {
		atomic.AddInt32(&starts, 1)
		return inner(ctx, name)
	})
	defer m.StopAll()
	a, err := m.Start(context.Background(), "same", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Start(context.Background(), "same", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	if a != b || atomic.LoadInt32(&starts) != 1 {
		t.Fatalf("same=%v starts=%d, want one shared session", a == b, starts)
	}
}

func TestUnchangedPicturesDoNotAdvanceTheSequence(t *testing.T) {
	// first frame, then the same picture twice, then a different one.
	m := NewManager(fakeHelper(t, "OK pid=1", []string{"FRAME 1 1 QQ==", "FRAME 1 1 QQ==", "FRAME 1 1 QQ==", "FRAME 1 1 Qg=="}))
	defer m.StopAll()
	oldInterval := frameInterval
	frameInterval = 10 * time.Millisecond
	defer func() { frameInterval = oldInterval }()

	s, err := m.Start(context.Background(), "seq", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.Latest()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	next, err := s.WaitFrame(ctx, first.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if string(next.JPEG) != "B" || next.Seq != first.Seq+1 {
		t.Fatalf("next = %q seq %d (first seq %d): identical frames must not count as new pictures", next.JPEG, next.Seq, first.Seq)
	}
}

func TestIdleSessionEndsItself(t *testing.T) {
	oldI, oldA, oldF := idleStop, activeWindow, frameInterval
	idleStop, activeWindow, frameInterval = 150*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond
	defer func() { idleStop, activeWindow, frameInterval = oldI, oldA, oldF }()

	m := NewManager(fakeHelper(t, "OK pid=1", []string{"FRAME 1 1 QQ=="}))
	s, err := m.Start(context.Background(), "idle", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
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

func TestStopAllEndsEverySession(t *testing.T) {
	m := NewManager(fakeHelper(t, "OK pid=1", []string{"FRAME 1 1 QQ=="}))
	s, err := m.Start(context.Background(), "a", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	m.StopAll()
	if s.Alive() {
		t.Fatal("session still alive after StopAll")
	}
}

func TestScriptIsEmbedded(t *testing.T) {
	if !strings.Contains(string(Script), "CreateDesktop") || strings.Contains(string(Script), "HttpListener") {
		t.Fatal("bridge.ps1 must create a private desktop and must not open a network listener")
	}
}

func TestAWatchedSessionIsNotEndedJustBecauseItsPictureIsStill(t *testing.T) {
	oldI, oldA, oldF := idleStop, activeWindow, frameInterval
	idleStop, activeWindow, frameInterval = 200*time.Millisecond, 60*time.Millisecond, 10*time.Millisecond
	defer func() { idleStop, activeWindow, frameInterval = oldI, oldA, oldF }()

	m := NewManager(fakeHelper(t, "OK pid=1", []string{"FRAME 1 1 QQ=="}))
	defer m.StopAll()
	s, err := m.Start(context.Background(), "watched", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Watch(ctx)

	// Well past idleStop, with a viewer connected and nothing changing on screen.
	time.Sleep(600 * time.Millisecond)
	if !s.Alive() {
		t.Fatal("a session with a viewer connected was ended for being idle: the app closes while someone is looking at it")
	}

	// Once the viewer goes away it is unwatched like any other and ends itself.
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for s.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("a session nobody watches any more was never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestModifiersAreValidatedBeforeTheyReachTheHost(t *testing.T) {
	m := NewManager(fakeHelper(t, "OK pid=1", []string{"FRAME 1 1 QQ=="}))
	defer m.StopAll()
	s, err := m.Start(context.Background(), "mods", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
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
	m := NewManager(fakeHelper(t, "OK pid=1", []string{"FRAME 1 1 QQ=="}))
	defer m.StopAll()
	s, err := m.Start(context.Background(), "paste", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
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
	log := t.TempDir() + "/received.log"
	t.Setenv("FAKE_LOG", log)
	script := `while IFS= read -r l; do
  printf '%s\n' "$l" >> "$FAKE_LOG"
  case "$l" in
    launch*) echo "OK pid=1";;
    frame)   echo "FRAME 1 1 QQ==";;
    "copy 67") echo "CLIP aGVsbG8gd29ybGQ=";;
    "copy 88") echo "CLIP";;
    quit)    exit 0;;
    *)       echo OK;;
  esac
done`
	m := NewManager(func(ctx context.Context, name string) (*exec.Cmd, error) {
		return exec.Command("bash", "-c", script), nil
	})
	defer m.StopAll()
	s, err := m.Start(context.Background(), "copy", `C:\a.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
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
