package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// A browser kept ready by an older build, and saved to be started again at every start, is never restored: run in place it
// would hold the person's own profile from the moment the control plane starts, and their own Chrome could not be opened
// (found live 2026-11-12). Any other kept app comes back as before.
func TestARestartDoesNotRestoreABrowserKeptReadyByAnOlderBuild(t *testing.T) {
	if !hostGUISupported() {
		t.Skip("restoring kept apps needs a computer that can run Windows apps")
	}
	dir := t.TempDir()
	data, _ := json.Marshal([]keptHostApp{
		{Desktop: "vm1", ID: "koustubh-your-chrome-chr-568721c6b768", Target: `C:\Users\me\Desktop\Koustubh (Your Chrome) - Chrome.lnk`},
		{Desktop: "vm1", ID: "microsoft-edge-1234567890ab", Target: `C:\edge.lnk`},
		{Desktop: "vm1", ID: "word-0123456789ab", Target: `C:\winword.exe`},
	})
	if err := os.WriteFile(dir+"/"+keptHostAppsFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{HostGUIDataDir: dir}
	s.hostGUIOnce.Do(func() {
		s.hostGUIMgr = winhost.NewManager(func(context.Context, string) (*exec.Cmd, error) { return nil, errors.New("no host in this test") })
	})
	t.Cleanup(func() { s.hostGUIMgr.StopAll() })
	s.hostAppDesktops = func(context.Context) ([]desktop.Desktop, error) { return nil, nil }

	s.RestoreHostApps(context.Background())

	s.keptHostAppsMu.Lock()
	defer s.keptHostAppsMu.Unlock()
	if _, ok := s.keptHostApps["koustubh-your-chrome-chr-568721c6b768"]; ok {
		t.Error("Chrome kept ready by an older build was restored")
	}
	if _, ok := s.keptHostApps["microsoft-edge-1234567890ab"]; ok {
		t.Error("Edge kept ready by an older build was restored")
	}
	if _, ok := s.keptHostApps["word-0123456789ab"]; !ok {
		t.Error("an ordinary kept app was not restored")
	}
	saved, _ := os.ReadFile(dir + "/" + keptHostAppsFile)
	var left []keptHostApp
	if err := json.Unmarshal(saved, &left); err != nil || len(left) != 1 || left[0].ID != "word-0123456789ab" {
		t.Errorf("the saved file must forget the browsers, so they stay gone: %s (%v)", saved, err)
	}
}

func TestSlugRunsInPlaceIsWholeWords(t *testing.T) {
	for slug, want := range map[string]bool{
		"koustubh-your-chrome-chrome": true, "google-chrome": true, "microsoft-edge": true, "brave": true,
		"knowledge-base": false, "chromebook-recovery-utility": false, "word": false,
	} {
		if got := desktop.SlugRunsInPlace(slug); got != want {
			t.Errorf("SlugRunsInPlace(%q) = %v, want %v", slug, got, want)
		}
	}
}
