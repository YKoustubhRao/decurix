package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

const wordCmd = `cmd.exe /c start "" "C:\Program Files\Microsoft Office\root\Office16\WINWORD.EXE"`

func bareManager() *Manager {
	return &Manager{missing: map[string][]MissingApp{}, retrying: map[string]bool{}}
}

// fastRetries shortens the background retry and replaces what it runs for the length of a test.
func fastRetries(t *testing.T, waits int, migrate func(m *Manager, ctx context.Context, id string, apps []string) error, gone func(m *Manager, ctx context.Context, id string) bool) *[][]MissingApp {
	t.Helper()
	oldDelays, oldMigrate, oldGone, oldWrite := hostAppRetryDelays, retryMigrate, desktopGone, writeMissingFileFn
	hostAppRetryDelays = make([]time.Duration, waits)
	for i := range hostAppRetryDelays {
		hostAppRetryDelays[i] = time.Millisecond
	}
	retryMigrate, desktopGone = migrate, gone
	var mu sync.Mutex
	written := &[][]MissingApp{}
	writeMissingFileFn = func(_ *Manager, _ context.Context, _ string, apps []MissingApp) error {
		mu.Lock()
		*written = append(*written, apps)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() {
		hostAppRetryDelays, retryMigrate, desktopGone, writeMissingFileFn = oldDelays, oldMigrate, oldGone, oldWrite
	})
	return written
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func notGone(*Manager, context.Context, string) bool { return false }

// A helper that died before answering is "not checked yet", which a later check may turn into a yes; the host's own
// "no", and a computer with no way to ask at all, are answers.
func TestNotCheckedIsKeptApartFromTheHostSayingNo(t *testing.T) {
	word := windowsApp("Word", wordCmd)
	store := windowsApp("Paint", `cmd.exe /c start explorer.exe shell:AppsFolder\Microsoft.Paint_8wekyb3d8bbwe!App`)

	useHostChecker(t, func(_ context.Context, targets []winhost.Target) (map[winhost.Target]string, error) {
		out := map[winhost.Target]string{}
		for _, tg := range targets {
			if strings.Contains(tg.Target, "Paint") {
				out[tg] = "This is a Microsoft Store app."
				return out, errors.New("the host helper is not running")
			}
		}
		return out, errors.New("the host helper is not running")
	})
	got := vetHostApps([]HostAppItem{store, word})
	if got[0].Unverified || got[0].Mode != ModeUnavailable || !strings.Contains(got[0].UnavailableReason, "Store") {
		t.Errorf("the host's no = %+v, want unavailable with its reason and not merely unchecked", got[0])
	}
	if !got[1].Unverified || got[1].Mode != ModeUnavailable || got[1].UnavailableReason != UnverifiedAppReason {
		t.Errorf("an app the host was never asked about = %+v, want unchecked", got[1])
	}

	useHostChecker(t, nil)
	if got := vetHostApps([]HostAppItem{word}); got[0].Unverified || got[0].Mode != ModeUnavailable {
		t.Errorf("with no way to ask the host = %+v, want a plain no", got[0])
	}
}

// "Not checked" is never remembered: the next look asks the host again, and an app it now accepts becomes usable.
func TestAnUncheckedAppIsAskedAboutAgainAndCanBecomeUsable(t *testing.T) {
	var calls int32
	useHostChecker(t, func(ctx context.Context, targets []winhost.Target) (map[winhost.Target]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, errors.New("the host helper stopped answering")
		}
		return allowEverything(ctx, targets)
	})
	word := windowsApp("Word", wordCmd)
	if got := vetHostApps([]HostAppItem{word}); !got[0].Unverified {
		t.Fatalf("first look = %+v, want unchecked", got[0])
	}
	hostAppsCacheMu.Lock()
	saved := cachedHostApps
	cachedHostApps = []HostAppItem{{ID: word.ID, Name: "Word", Mode: ModeUnavailable, Unverified: true, HostCmd: wordCmd, AppType: "windows_app"}}
	hostAppsCacheMu.Unlock()
	t.Cleanup(func() { hostAppsCacheMu.Lock(); cachedHostApps = saved; hostAppsCacheMu.Unlock() })

	got := recheckHostApps([]HostAppItem{word})
	if got[0].Unverified || got[0].Mode != ModeHostGUI || got[0].UnavailableReason != "" {
		t.Fatalf("second look = %+v, want usable", got[0])
	}
	hostAppsCacheMu.RLock()
	cached := cachedHostApps[0]
	hostAppsCacheMu.RUnlock()
	if cached.Unverified || cached.Mode != ModeHostGUI {
		t.Errorf("the cached list still says %+v", cached)
	}
}

// The create request is answered at once with what is already known about the requested apps, without asking anyone.
func TestTheCreateAnswerSaysWhatIsAlreadyKnownMissing(t *testing.T) {
	hostAppsCacheMu.Lock()
	saved := cachedHostApps
	cachedHostApps = []HostAppItem{
		{ID: "win_paint", Name: "Paint", Mode: ModeUnavailable, UnavailableReason: "This is a Microsoft Store app."},
		{ID: "win_word", Name: "Word", Mode: ModeUnavailable, Unverified: true, UnavailableReason: UnverifiedAppReason},
		{ID: "win_excel", Name: "Excel", Mode: ModeHostGUI},
	}
	hostAppsCacheMu.Unlock()
	t.Cleanup(func() { hostAppsCacheMu.Lock(); cachedHostApps = saved; hostAppsCacheMu.Unlock() })

	got := PrecheckHostApps([]string{"win_excel", "WIN_PAINT", "win_word", "win_unknown"})
	if len(got) != 2 {
		t.Fatalf("got %+v, want Paint and Word", got)
	}
	if got[0].Name != "Paint" || got[0].Pending || !strings.Contains(got[0].Reason, "Store") {
		t.Errorf("Paint = %+v", got[0])
	}
	if got[1].Name != "Word" || !got[1].Pending {
		t.Errorf("Word = %+v, want pending", got[1])
	}
}

// One migration settles the apps it was asked about - added ones leave the list - and leaves the others alone.
func TestAMigrationSettlesOnlyTheAppsItWasAskedAbout(t *testing.T) {
	m := bareManager()
	m.noteMissingApps("pc", map[string]bool{"win_word": true, "win_paint": true}, []MissingApp{
		{ID: "win_word", Name: "Word", Reason: UnverifiedAppReason, Pending: true},
		{ID: "win_paint", Name: "Paint", Reason: "This is a Microsoft Store app."},
	})
	if got := m.MissingApps("pc"); len(got) != 2 || got[0].Name != "Paint" || got[1].Name != "Word" {
		t.Fatalf("recorded %+v, want both, by name", got)
	}
	if !m.noteMissingApps("pc", map[string]bool{"win_word": true}, nil) {
		t.Error("the agent's file was not rewritten after Word was added")
	}
	if got := m.MissingApps("pc"); len(got) != 1 || got[0].Name != "Paint" {
		t.Errorf("after Word was added: %+v, want only Paint", got)
	}
	if m.noteMissingApps("other", map[string]bool{"win_excel": true}, nil) {
		t.Error("a desktop that never missed anything had its file written")
	}
}

// An app the host could not be asked about is tried again in the background and, once the check passes, added to the
// desktop as it is - nothing recreated - and drops off the list.
func TestAPendingAppIsRetriedAndAddedWhenItChecksOut(t *testing.T) {
	m := bareManager()
	var tries int32
	fastRetries(t, 5, func(m *Manager, _ context.Context, id string, apps []string) error {
		if len(apps) != 1 || apps[0] != "win_word" {
			t.Errorf("retried %v, want only the pending app", apps)
		}
		if atomic.AddInt32(&tries, 1) < 3 {
			return nil // still not checked: stays pending
		}
		m.noteMissingApps(id, map[string]bool{"win_word": true}, nil)
		return nil
	}, notGone)

	m.noteMissingApps("pc", map[string]bool{"win_word": true, "win_paint": true}, []MissingApp{
		{ID: "win_word", Name: "Word", Reason: UnverifiedAppReason, Pending: true},
		{ID: "win_paint", Name: "Paint", Reason: "This is a Microsoft Store app."},
	})
	m.retryPendingApps("pc")
	m.retryPendingApps("pc") // a second start while one runs is a no-op
	waitFor(t, "the retry to finish", func() bool {
		m.missingMu.Lock()
		defer m.missingMu.Unlock()
		return !m.retrying["pc"]
	})
	if n := atomic.LoadInt32(&tries); n != 3 {
		t.Errorf("tries = %d, want 3 (it stops once the app is added)", n)
	}
	if got := m.MissingApps("pc"); len(got) != 1 || got[0].Name != "Paint" {
		t.Errorf("left %+v, want only the app the host refused", got)
	}
}

// Retries are bounded: after the last, a still-unchecked app is reported as not added, and the agent is told.
func TestRetriesEndInAPlainNotAdded(t *testing.T) {
	m := bareManager()
	var tries int32
	written := fastRetries(t, 3, func(*Manager, context.Context, string, []string) error {
		atomic.AddInt32(&tries, 1)
		return nil
	}, notGone)
	m.noteMissingApps("pc", map[string]bool{"win_word": true}, []MissingApp{{ID: "win_word", Name: "Word", Reason: UnverifiedAppReason, Pending: true}})
	m.retryPendingApps("pc")
	waitFor(t, "the retry to give up", func() bool { return len(m.pendingApps("pc")) == 0 })
	waitFor(t, "the agent's file", func() bool {
		m.missingMu.Lock()
		defer m.missingMu.Unlock()
		return !m.retrying["pc"]
	})
	if n := atomic.LoadInt32(&tries); n != 3 {
		t.Errorf("tries = %d, want 3", n)
	}
	got := m.MissingApps("pc")
	if len(got) != 1 || got[0].Pending || got[0].Reason != gaveUpReason {
		t.Errorf("after the retries: %+v, want Word plainly not added", got)
	}
	if len(*written) != 1 || len((*written)[0]) != 1 || (*written)[0][0].Pending {
		t.Errorf("agent told %+v, want the final word once", *written)
	}
}

// A desktop deleted while its apps were pending stops the retry and leaves nothing behind.
func TestADeletedDesktopStopsItsRetry(t *testing.T) {
	m := bareManager()
	fastRetries(t, 3, func(*Manager, context.Context, string, []string) error {
		t.Error("migrated into a desktop that is gone")
		return nil
	}, func(*Manager, context.Context, string) bool { return true })
	m.noteMissingApps("pc", map[string]bool{"win_word": true}, []MissingApp{{ID: "win_word", Name: "Word", Reason: UnverifiedAppReason, Pending: true}})
	m.retryPendingApps("pc")
	waitFor(t, "the record to be dropped", func() bool { return m.MissingApps("pc") == nil })
}

// The file the desktop's agent reads round-trips, and an empty list is written as [] (clearing an old one).
func TestTheAgentsFileRoundTrips(t *testing.T) {
	apps := []MissingApp{{ID: "win_word", Name: "Word", Reason: UnverifiedAppReason, Pending: true}}
	data, err := missingFileTar(apps)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if h.Uid != 1000 {
			t.Errorf("%s is owned by %d, want the desktop's user", h.Name, h.Uid)
		}
	}
	if len(names) != 2 || names[1] != MissingAppsFile {
		t.Errorf("tar holds %v", names)
	}
	got, err := readMissingTar(bytes.NewReader(data))
	if err != nil || len(got) != 1 || got[0] != apps[0] {
		t.Errorf("read back %+v, %v", got, err)
	}

	empty, _ := missingFileTar(nil)
	if got, err := readMissingTar(bytes.NewReader(empty)); err != nil || got == nil || len(got) != 0 {
		t.Errorf("an empty list read back as %#v, %v; want []", got, err)
	}
}

// An app the host could not keep running (it closed straight away, again and again) is recorded where the card and the
// desktop's agent see what it lacks, with the reason; kept again, the record goes.
func TestAnAppTheHostCannotKeepRunningIsRecordedAndClearedWhenKeptAgain(t *testing.T) {
	written := fastRetries(t, 0, nil, notGone)
	m := bareManager()
	m.MarkAppUnavailable("vm1", "notepad", "Notepad", "It cannot run here: on this computer it closed straight away.")
	got := m.MissingApps("vm1")
	if len(got) != 1 || got[0].Name != "Notepad" || got[0].ID != "kept:notepad" || got[0].Pending ||
		!strings.Contains(got[0].Reason, "closed straight away") {
		t.Fatalf("missing = %+v", got)
	}
	waitFor(t, "the agent's file to be written", func() bool { return len(*written) > 0 })
	m.ClearAppUnavailable("vm1", "notepad")
	if got := m.MissingApps("vm1"); len(got) != 0 {
		t.Fatalf("still missing after it was kept again: %+v", got)
	}
	// The agent's file is written again, empty (and before this test's stand-in writer is put back).
	waitFor(t, "the agent's file to be cleared", func() bool { return len(*written) > 1 })
	m.ClearAppUnavailable("vm1", "paint") // nothing recorded: nothing to do
}

// A copy into the home that failed as a whole (a new home still root-owned, found 2026-11-12) leaves every requested
// app missing and says so; an app already missing for its own reason keeps that reason, and the bridge that was never
// asked for is not listed.
func TestAFailedCopyListsEveryRequestedAppAsMissing(t *testing.T) {
	requested := map[string]bool{"koustubh-chrome": true, "paint": true}
	already := []MissingApp{{ID: "paint", Name: "Paint", Reason: "The host said no."}}
	got := missingAfterFailedCopy(requested, already, map[string]string{"koustubh-chrome": "Koustubh - Chrome", "host_bridge": "Host bridge"})
	if len(got) != 2 {
		t.Fatalf("want 2 missing apps, got %+v", got)
	}
	if got[0].ID != "paint" || got[0].Reason != "The host said no." {
		t.Errorf("an app already missing keeps its own reason: %+v", got[0])
	}
	if got[1].ID != "koustubh-chrome" || got[1].Name != "Koustubh - Chrome" || got[1].Reason != copyFailedReason || got[1].Pending {
		t.Errorf("the app the failed copy lost: %+v", got[1])
	}

	m := bareManager()
	m.noteMissingApps("vm1", requested, got)
	if n := len(m.MissingApps("vm1")); n != 2 {
		t.Errorf("the desktop's card must show both, got %d", n)
	}
}
