package winhost

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// A mirror names a program, and only a program: nothing with a path or anything else a helper line could be steered with.
func TestAMirrorMustNameAProgram(t *testing.T) {
	for _, bad := range []string{"mirror:", "mirror:../evil", `mirror:C:\Windows\notepad.exe`, "mirror:a b", "mirror:a\tb",
		"mirror:" + strings.Repeat("x", 65), "chrome"} {
		if line, err := mirrorLine(bad, ""); err == nil && IsMirror(bad) {
			t.Errorf("%q was taken for a program: %q", bad, line)
		}
	}
	line, err := mirrorLine(MirrorTarget("chrome"), "  WhatsApp ")
	if err != nil || line != "attach\tchrome\tWhatsApp" {
		t.Fatalf("got %q, %v", line, err)
	}
	if IsMirror("chrome") || !IsMirror(MirrorTarget("msedge")) {
		t.Error("IsMirror misreads a target")
	}
}

// Nothing is started for a mirror, so nothing is asked of the host about it, and it is never kept ready.
func TestAMirrorIsNeitherCheckedNorKept(t *testing.T) {
	var starts int32
	m := NewManager(func(context.Context, string) (*exec.Cmd, error) {
		atomic.AddInt32(&starts, 1)
		return nil, errors.New("the host must not be asked")
	})
	t.Cleanup(m.StopAll)

	good, bad := Target{MirrorTarget("chrome"), ""}, Target{"mirror:../x", ""}
	verdicts, err := m.Check(context.Background(), []Target{good, bad})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := verdicts[good]; !ok || v != "" {
		t.Errorf("a mirror of a program must be fine, got %q (answered: %v)", v, ok)
	}
	if v := verdicts[bad]; v == "" {
		t.Error("a mirror of something that is not a program must be refused")
	}
	m.Keep("vm1", "vm1-chrome", good.Target, good.Args)
	if got := m.KeptFor("vm1"); len(got) != 0 {
		t.Errorf("a mirror was kept ready: %v", got)
	}
	if n := atomic.LoadInt32(&starts); n != 0 {
		t.Errorf("a helper was started %d times for a mirror", n)
	}
}

// A mirror is attached to, never launched, and no process of ours is behind it.
func TestAMirrorIsAttachedNotLaunched(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{launch: "OK pid=0"})
	s, err := m.Start(context.Background(), "vm1-chrome", MirrorTarget("chrome"), "WhatsApp")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(received(t), "|")
	if !strings.Contains(got, "attach\tchrome\tWhatsApp") {
		t.Errorf("the helper was never asked to attach: %q", got)
	}
	if strings.Contains(got, "launch") {
		t.Errorf("a mirror was launched: %q", got)
	}
	if s.appPID != 0 {
		t.Errorf("a mirror has an app process %d: it would be ended with the helper", s.appPID)
	}
}
