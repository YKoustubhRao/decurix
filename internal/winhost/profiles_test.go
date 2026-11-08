package winhost

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestProfileNamesRefuseAnythingButAPlainSessionID: only a session id the host's folder names keep whole, with nothing a
// path could be steered with, ever names a profile to remove (B-19).
func TestProfileNamesRefuseAnythingButAPlainSessionID(t *testing.T) {
	long40 := strings.Repeat("a", 40)
	names, refused := profileNames([]string{"vm1-a", "vm1-a", "files-0123456789ab", long40,
		"../x", `a\b`, "a/b", "a.b", "..", "", "-a", "a b", "a\tb", strings.Repeat("a", 41), "vm1-é"})
	want := []string{"decurix_" + long40, "decurix_files_0123456789ab", "decurix_vm1_a"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
	if len(refused) != 11 {
		t.Fatalf("refused %d ids (%q), want the 11 of the wrong shape", len(refused), refused)
	}
	for _, n := range names {
		if !profileOwner.MatchString(n) || strings.ContainsAny(n, `-./\`) {
			t.Fatalf("%q is not a name the bridge accepts", n)
		}
	}
}

// TestTheBridgeRemovesProfilesWithoutFollowingLinks pins what bridge.ps1's dropprofiles is made of: its own walk, which
// never enters a link, and never Remove-Item, which in Windows PowerShell 5.1 walks into a junction and empties its target.
func TestTheBridgeRemovesProfilesWithoutFollowingLinks(t *testing.T) {
	src := string(Script)
	start := strings.Index(src, "function Remove-TreeNoFollow")
	end := strings.Index(src, "function Format-Check")
	if start < 0 || end < start {
		t.Fatal("bridge.ps1 lost Remove-TreeNoFollow / Remove-DesktopProfiles")
	}
	body := src[start:end]
	for _, want := range []string{"function Remove-DesktopProfiles", "ReparsePoint", "GetFullPath", "OrdinalIgnoreCase",
		`'^decurix_[A-Za-z0-9_]{1,40}$'`, "IndexOf('-')"} {
		if !strings.Contains(body, want) {
			t.Errorf("Remove-DesktopProfiles lost %q", want)
		}
	}
	if strings.Contains(body, "Remove-Item") {
		t.Error("profiles must be removed by the walk that never follows a link, not Remove-Item")
	}
	if !strings.Contains(src, `"dropprofiles" {`) || !strings.Contains(src, "Remove-DesktopProfiles $rest.Split(") {
		t.Error("bridge.ps1 no longer answers dropprofiles")
	}
	if profileOwner.String() != "^decurix_[A-Za-z0-9_]{1,40}$" {
		t.Error("the Go check of a profile owner's name and bridge.ps1's must be the same")
	}
}

func dropLines(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, l := range received(t) {
		if strings.HasPrefix(l, "dropprofiles") {
			out = append(out, l)
		}
	}
	return out
}

// TestDeletingAVMRemovesItsAppsProfilesOnTheHost: the VM's apps are ended first, then one dropprofiles names exactly the
// private desktops of its apps - those kept here and those the caller knows - and nobody else's.
func TestDeletingAVMRemovesItsAppsProfilesOnTheHost(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})
	m.Keep("vm1", "vm1-a", `C:\a.exe`, "")
	m.Keep("vm2", "vm2-a", `C:\a.exe`, "")
	waitFor(t, "kept apps were not started", func() bool { return m.Alive("vm1-a") && m.Alive("vm2-a") })
	s, _ := m.Get("vm1-a")

	removed, err := m.DeleteDesktop("vm1", []string{"vm1-b"})
	if err != nil || removed != 1 {
		t.Fatalf("DeleteDesktop = %d, %v", removed, err)
	}
	if s.Alive() {
		t.Fatal("the app of a deleted VM was still running when its profile was removed")
	}
	if got := dropLines(t); len(got) != 1 || got[0] != "dropprofiles\tdecurix_vm1_a\tdecurix_vm1_b" {
		t.Fatalf("sent %q, want one dropprofiles naming vm1's two apps", got)
	}
	if !m.Alive("vm2-a") {
		t.Error("deleting one VM ended another's app")
	}
}

// TestADeleteNeverSendsAnIDOfTheWrongShape: a VM id or session id that could steer a folder name is refused before the
// host hears of it.
func TestADeleteNeverSendsAnIDOfTheWrongShape(t *testing.T) {
	m, _ := fakeManager(t, fakeOpts{})
	if _, err := m.DeleteDesktop("../vm1", []string{"vm1-a"}); err == nil {
		t.Fatal("a VM id with a path in it was accepted")
	}
	if _, err := m.DeleteDesktop("vm1", []string{"../x", `a\b`, "a.b", strings.Repeat("a", 41)}); err == nil {
		t.Fatal("session ids of the wrong shape were not reported")
	}
	if got := dropLines(t); len(got) != 0 {
		t.Fatalf("sent %q for ids of the wrong shape", got)
	}
	if _, err := m.DeleteDesktop("vm1", []string{"../x", "vm1-a"}); err == nil {
		t.Fatal("the refused id was not reported")
	}
	if got := dropLines(t); len(got) != 1 || got[0] != "dropprofiles\tdecurix_vm1_a" {
		t.Fatalf("sent %q, want only the good id", got)
	}
}

// TestAProfileStillHeldOpenIsAskedForAgain: a browser's own processes can hold a file a moment after the app ended; the
// removal is asked for again, a bounded number of times, and what is left is reported.
func TestAProfileStillHeldOpenIsAskedForAgain(t *testing.T) {
	oldTries, oldRetry := profileTries, profileRetry
	profileTries, profileRetry = 3, 10*time.Millisecond
	defer func() { profileTries, profileRetry = oldTries, oldRetry }()
	m, _ := fakeManager(t, fakeOpts{held: true})
	_, err := m.DeleteDesktop("vm1", []string{"vm1-a"})
	if err == nil || !strings.Contains(err.Error(), "held open") {
		t.Fatalf("err = %v, want the profile still held open", err)
	}
	if got := dropLines(t); len(got) != 3 {
		t.Fatalf("asked %d times, want 3", len(got))
	}
}

// TestSettleLetsAStartInFlightFinishAndEndsIt: a kept app being started again as its VM is deleted must not be left
// holding the profile that is about to be removed.
func TestSettleLetsAStartInFlightFinishAndEndsIt(t *testing.T) {
	m := NewManager(nil)
	c := &startCall{done: make(chan struct{})}
	m.starting["vm1-a"] = c
	done := make(chan struct{})
	go func() {
		m.settle(context.Background(), "vm1-a")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("settle returned while a start was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	m.mu.Lock()
	delete(m.starting, "vm1-a")
	m.mu.Unlock()
	close(c.done)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("settle did not return once the start finished")
	}
}
