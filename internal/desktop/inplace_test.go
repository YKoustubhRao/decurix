package desktop

import (
	"archive/tar"
	"net/url"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

const lnkCmd = `cmd.exe /c start "" "C:\Users\me\Desktop\Koustubh (Your Chrome) - Chrome.lnk"`

func TestOnlyABrowserRunsInPlace(t *testing.T) {
	for name, want := range map[string]bool{
		"Google Chrome":                   true,
		"Koustubh (Your Chrome) - Chrome": true,
		"Microsoft Edge":                  true,
		"Brave":                           true,
		"Knowledge Base":                  false, // "edge" inside a word
		"Chromebook Recovery Utility":     false, // "chrome" inside a word
		"Word":                            false,
		"Notepad":                         false,
	} {
		if got := runsInPlace(HostAppItem{Name: name, AppType: "windows_app", HostCmd: lnkCmd}); got != want {
			t.Errorf("runsInPlace(%q) = %v, want %v", name, got, want)
		}
	}
	if runsInPlace(HostAppItem{Name: "Google Chrome", AppType: "browser_session"}) {
		t.Error("the data entry for Chrome's files is not a program to run")
	}
}

// Every VM that opens an in-place browser joins one host session (one Chrome, which can hold the profile once); any other app
// is a session per VM, as ever.
func TestEveryVMSharesOneSessionForABrowserRunInPlace(t *testing.T) {
	a, b := packContext{DesktopID: "vm-a", Shared: true}, packContext{DesktopID: "vm-b", Shared: true}
	if a.sessionID("google-chrome") != b.sessionID("google-chrome") {
		t.Errorf("two VMs got two sessions for the one Chrome: %s, %s", a.sessionID("google-chrome"), b.sessionID("google-chrome"))
	}
	if a.sessionID("google-chrome") == a.sessionID("microsoft-edge") {
		t.Error("two browsers share a session")
	}
	a.Shared, b.Shared = false, false
	if a.sessionID("word") == b.sessionID("word") {
		t.Error("two VMs share a session for an ordinary app")
	}
}

// A browser's launcher is signed for the shared session and nothing is started ahead; another app is started ahead as before.
func TestAnInPlaceBrowserIsSharedAndNeverStartedAhead(t *testing.T) {
	var started []string
	SetHostAppStarter(func(_, id, target, _ string) { started = append(started, id+" "+target) })
	t.Cleanup(func() { SetHostAppStarter(nil) })

	launcherQuery := func(vm string, app HostAppItem) url.Values {
		pc := packContext{DesktopID: vm, Secret: testPack.Secret}
		files := pack(t, func(tw *tar.Writer) error { return packDynamicApp(tw, app, pc) })
		for name, f := range files {
			if !strings.HasPrefix(name, ".local/bin/") || !strings.HasSuffix(name, "-host") {
				continue
			}
			wrapper := string(f.data)
			i := strings.Index(wrapper, "host-gui/view?")
			if i < 0 {
				continue
			}
			q := wrapper[i+len("host-gui/view?"):]
			v, err := url.ParseQuery(q[:strings.IndexAny(q, "\"\n")])
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		t.Fatalf("no launcher for %s", app.Name)
		return nil
	}

	chrome := windowsApp("Google Chrome", `cmd.exe /c start "" "C:\Program Files\Google\Chrome\Application\chrome.exe"`)
	one, two := launcherQuery("vm-a", chrome), launcherQuery("vm-b", chrome)
	if one.Get("id") != two.Get("id") || one.Get("token") != two.Get("token") {
		t.Errorf("two VMs' launchers differ in session or token: %v / %v", one, two)
	}
	if !winhost.ValidToken(testPack.Secret, one.Get("id"), one.Get("target"), one.Get("args"), one.Get("token")) {
		t.Error("the shared launcher's token is not valid for its own session")
	}
	if one.Get("desktop") != "vm-a" || two.Get("desktop") != "vm-b" {
		t.Error("the launcher must still say which VM is viewing (idle-pause counts it as in use)")
	}
	if len(started) != 0 {
		t.Errorf("a browser run in place was started ahead: %v", started)
	}

	word := windowsApp("Word", wordCmd)
	if a, b := launcherQuery("vm-a", word), launcherQuery("vm-b", word); a.Get("id") == b.Get("id") {
		t.Error("an ordinary app is one session for two VMs")
	}
	if len(started) != 2 {
		t.Errorf("an ordinary app is still started ahead, once per VM: %v", started)
	}
}
