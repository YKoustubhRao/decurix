package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// quietHostServer is a Server whose host manager can start nothing (no Windows helper in a test), over the given data dir.
func quietHostServer(t *testing.T, dir string) *Server {
	t.Helper()
	s := &Server{HostGUIDataDir: dir}
	s.hostGUIOnce.Do(func() {
		s.hostGUIMgr = winhost.NewManager(func(context.Context, string) (*exec.Cmd, error) {
			return nil, errors.New("no host in this test")
		})
	})
	t.Cleanup(func() { s.hostGUIMgr.StopAll() })
	s.hostAppDesktops = func(context.Context) ([]desktop.Desktop, error) { return nil, nil } // none running: nothing is kept
	return s
}

// The person's choice to bring an app's sign-ins is kept with the kept app, so a restart of the control plane does not
// turn it off (an app then launched without it would have its imported profile deleted).
func TestTheSignInChoiceSurvivesARestartOfTheControlPlane(t *testing.T) {
	dir := t.TempDir()
	s := quietHostServer(t, dir)
	s.keptHostApps = map[string]keptHostApp{
		"vm1-chrome": {Desktop: "vm1", ID: "vm1-chrome", Target: `C:\chrome.exe`},
		"vm1-edge":   {Desktop: "vm1", ID: "vm1-edge", Target: `C:\msedge.exe`},
	}

	s.noteHostAppSignIns("vm1-chrome", true)
	s.noteHostAppSignIns("vm1-edge", true)
	s.noteHostAppSignIns("vm1-edge", false) // unticked later

	data, err := os.ReadFile(dir + "/" + keptHostAppsFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved []keptHostApp
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, app := range saved {
		got[app.ID] = app.SignIns
	}
	if !got["vm1-chrome"] || got["vm1-edge"] {
		t.Fatalf("saved choices = %v, want chrome on and edge off", got)
	}

	if !hostGUISupported() {
		t.Skip("restoring kept apps needs a computer that can run Windows apps")
	}
	again := quietHostServer(t, dir)
	again.RestoreHostApps(context.Background())
	again.keptHostAppsMu.Lock()
	defer again.keptHostAppsMu.Unlock()
	if !again.hostSignIns["vm1-chrome"] || again.hostSignIns["vm1-edge"] {
		t.Fatalf("after a restart the choices are %v, want chrome on and edge off", again.hostSignIns)
	}
}

// Like the apps themselves, bringing sign-ins is chosen on this computer's own screen: never through the phone or tunnel
// address.
func TestTheGatedListenerCannotChooseToBringSignIns(t *testing.T) {
	srv := &Server{Desktops: fakeDesktopManager(t)}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	gated := MarkGated(mux)

	rec := createWith(t, gated, url.Values{"name": {"signin-lan"}, "profile": {"basic"}, "brain": {"ollama"},
		"signin_apps": {"win_google-chrome"}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only be chosen on this computer") {
		t.Fatalf("through the gated listener: %d %s", rec.Code, rec.Body.String())
	}
	if _, made := srv.Desktops.GetProvisioning("signin-lan"); made {
		t.Fatal("a desktop was started with sign-ins chosen through the gated listener")
	}
}

// A creation that records the choice first and is then refused puts back what the desktop of that name had.
func TestARefusedCreationLeavesTheEarlierSignInChoiceAlone(t *testing.T) {
	srv := &Server{Desktops: fakeDesktopManager(t)}
	srv.Desktops.SetSignInApps("signin-kept", []string{"win_google-chrome"}, []string{"win_google-chrome"})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	rec := createWith(t, mux, url.Values{"name": {"signin-kept"}, "profile": {"no-such-profile"}, "brain": {"ollama"},
		"copy_host_apps": {"1"}, "host_apps": {"win_google-chrome"}, "signin_apps": {""}})
	if rec.Code == http.StatusAccepted {
		t.Skip("this stand-in accepted the profile; nothing was refused to test the put-back on")
	}
	if !srv.Desktops.SignInApps("signin-kept")["win_google-chrome"] {
		t.Fatal("a refused creation cleared an earlier choice")
	}
}
