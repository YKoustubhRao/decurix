package desktop

import (
	"archive/tar"
	"net/url"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// One "live on this PC" entry per browser program, however many of its shortcuts were found; not for an app whose name
// only contains a browser's as part of a longer word, and not for what is not a Windows program.
func TestOneMirrorEntryPerBrowserAndNoFalseOnes(t *testing.T) {
	cmd := `cmd.exe /c start "" "C:\Users\me\Desktop\x.lnk"`
	items := []HostAppItem{
		{ID: "win_google-chrome", Name: "Google Chrome", AppType: "windows_app", HostCmd: cmd, Icon: "chrome-icon"},
		{ID: "win_koustubh-your-chrome-chrome", Name: "Koustubh (Your Chrome) - Chrome", AppType: "windows_app", HostCmd: cmd},
		{ID: "win_microsoft-edge", Name: "Microsoft Edge", AppType: "windows_app", HostCmd: cmd},
		{ID: "win_knowledge-base", Name: "Knowledge Base", AppType: "windows_app", HostCmd: cmd}, // "edge" inside a word
		{ID: "win_word", Name: "Word", AppType: "windows_app", HostCmd: cmd},
		{ID: "chrome", Name: "Google Chrome", AppType: "browser_session"}, // data, not a program
	}
	got := mirrorApps(items)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "mirror_chrome,mirror_msedge" {
		t.Fatalf("mirror entries = %v, want one for Chrome and one for Edge", ids)
	}
	chrome := got[0]
	if chrome.Mode != ModeHostGUI || !chrome.Detected || chrome.Icon != "chrome-icon" || chrome.UnavailableReason != "" {
		t.Errorf("a mirror entry must be ready to pick as it is: %+v", chrome)
	}
	if !strings.Contains(chrome.Description, "Nothing is copied") || !strings.Contains(chrome.Description, "acts as") {
		t.Errorf("the entry must say what it is and what it costs the person: %q", chrome.Description)
	}
	target, args, ok := windowsTarget(chrome.HostCmd)
	if !ok || target != "mirror:chrome" || args != "" || !winhost.IsMirror(target) {
		t.Errorf("the entry's command must read back as a mirror of chrome: %q %q %v", target, args, ok)
	}
}

// A mirror's launcher is signed for exactly that program, opens the viewer like any other, and no app is started ahead:
// the person's own browser is already open, or the launcher says it is not.
func TestAMirrorLauncherIsSignedAndStartsNothingAhead(t *testing.T) {
	var started []string
	SetHostAppStarter(func(_, id, target, _ string) { started = append(started, id+" "+target) })
	t.Cleanup(func() { SetHostAppStarter(nil) })

	app := mirrorApps([]HostAppItem{{ID: "win_google-chrome", Name: "Google Chrome", AppType: "windows_app",
		HostCmd: `cmd.exe /c start "" "C:\x.lnk"`}})[0]
	files := pack(t, func(tw *tar.Writer) error { return packDynamicApp(tw, app, testPack) })

	var wrapper string
	for name, f := range files {
		if strings.HasPrefix(name, ".local/bin/") && strings.HasSuffix(name, "-host") {
			wrapper = string(f.data)
		}
	}
	if wrapper == "" {
		t.Fatalf("no launcher was written: %v", len(files))
	}
	i := strings.Index(wrapper, "host-gui/view?")
	if i < 0 {
		t.Fatalf("the launcher does not open the viewer:\n%s", wrapper)
	}
	query := wrapper[i+len("host-gui/view?"):]
	query = query[:strings.IndexAny(query, "\"\n")]
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("target") != "mirror:chrome" {
		t.Errorf("the viewer is asked for target %q, want mirror:chrome", q.Get("target"))
	}
	if !winhost.ValidToken(testPack.Secret, q.Get("id"), "mirror:chrome", q.Get("args"), q.Get("token")) {
		t.Error("the launcher's token is not valid for exactly this mirror")
	}
	if winhost.ValidToken(testPack.Secret, q.Get("id"), "mirror:msedge", q.Get("args"), q.Get("token")) {
		t.Error("the token for Chrome also opens Edge")
	}
	if len(started) != 0 {
		t.Errorf("an app was started ahead for a mirror: %v", started)
	}
}
