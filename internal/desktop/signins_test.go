package desktop

import (
	"archive/tar"
	"reflect"
	"sort"
	"testing"
)

const chromeCmd = `cmd.exe /c start "" "C:\Program Files\Google\Chrome\Application\chrome.exe"`

func ids(set map[string]bool) []string {
	var out []string
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// The choice is per app and per desktop: ticking one browser changes no other, and copying one more app to a desktop
// later keeps what was chosen for the earlier ones.
func TestSignInChoiceIsPerAppAndKeptAcrossLaterCopies(t *testing.T) {
	m := bareManager()
	m.SetSignInApps("work-vm", []string{"win_chrome", "win_edge"}, []string{"win_chrome"})
	if got := ids(m.signInAppsOf("work-vm")); !reflect.DeepEqual(got, []string{"win_chrome"}) {
		t.Fatalf("only the ticked browser brings its sign-ins, got %v", got)
	}

	// Edge copied again later, now ticked: Chrome's choice stays.
	m.SetSignInApps("work-vm", []string{"win_edge"}, []string{"win_edge"})
	if got := ids(m.signInAppsOf("work-vm")); !reflect.DeepEqual(got, []string{"win_chrome", "win_edge"}) {
		t.Fatalf("a later copy must keep the earlier choice, got %v", got)
	}

	// Chrome copied again, not ticked this time: that is the new choice for Chrome alone.
	m.SetSignInApps("work-vm", []string{"win_chrome"}, nil)
	if got := ids(m.signInAppsOf("work-vm")); !reflect.DeepEqual(got, []string{"win_edge"}) {
		t.Fatalf("an unticked app must stop bringing its sign-ins, got %v", got)
	}

	// A tick for an app that is not being given to the desktop counts for nothing.
	m.SetSignInApps("other-vm", []string{"win_edge"}, []string{"win_chrome"})
	if got := ids(m.signInAppsOf("other-vm")); len(got) != 0 {
		t.Fatalf("a tick on an app not requested must be ignored, got %v", got)
	}
	if got := ids(m.signInAppsOf("work-vm")); !reflect.DeepEqual(got, []string{"win_edge"}) {
		t.Fatalf("another desktop's choice must not leak, got %v", got)
	}

	// A refused creation puts back what was there.
	prior := m.SignInApps("work-vm")
	m.SetSignInApps("work-vm", []string{"win_edge"}, nil)
	m.ReplaceSignInApps("work-vm", prior)
	if got := ids(m.signInAppsOf("work-vm")); !reflect.DeepEqual(got, []string{"win_edge"}) {
		t.Fatalf("restoring must bring the earlier choice back, got %v", got)
	}
}

// Only a browser can bring sign-ins, and only when it was ticked for this desktop.
func TestOnlyATickedBrowserBringsSignIns(t *testing.T) {
	pc := packContext{DesktopID: "work-vm", SignIns: map[string]bool{"win_google-chrome": true, "win_word": true}}
	if !bringsSignIns(pc, HostAppItem{ID: "win_google-chrome", Name: "Google Chrome"}) {
		t.Error("a ticked browser must bring its sign-ins")
	}
	if bringsSignIns(pc, HostAppItem{ID: "win_word", Name: "Word"}) {
		t.Error("an app that is not a browser has no profile to seed")
	}
	if bringsSignIns(pc, HostAppItem{ID: "win_microsoft-edge", Name: "Microsoft Edge"}) {
		t.Error("a browser that was not ticked must not")
	}
	if bringsSignIns(packContext{}, HostAppItem{ID: "win_google-chrome", Name: "Google Chrome"}) {
		t.Error("with no choice at all nothing is brought")
	}
}

// Packing tells the choice before the app is started ahead, for the app's own session id, so that first start has it.
func TestPackingTellsTheChoiceBeforeTheAppStartsAhead(t *testing.T) {
	var events []string
	SetHostAppSignIns(func(id string, on bool) {
		if on {
			events = append(events, "signins-on:"+id)
		} else {
			events = append(events, "signins-off:"+id)
		}
	})
	SetHostAppStarter(func(desktopID, id, target, args string) { events = append(events, "start:"+id) })
	t.Cleanup(func() { SetHostAppSignIns(nil); SetHostAppStarter(nil) })

	chrome := windowsApp("Google Chrome", chromeCmd)
	pc := testPack
	pc.SignIns = map[string]bool{chrome.ID: true}
	pack(t, func(tw *tar.Writer) error { return packDynamicApp(tw, chrome, pc) })

	id := pc.sessionID("google-chrome")
	want := []string{"signins-on:" + id, "start:" + id}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("got %v, want %v", events, want)
	}

	events = nil
	pack(t, func(tw *tar.Writer) error { return packDynamicApp(tw, chrome, testPack) }) // not ticked this time
	if want := []string{"signins-off:" + id, "start:" + id}; !reflect.DeepEqual(events, want) {
		t.Fatalf("an unticked app must be told off before it starts: got %v, want %v", events, want)
	}
}
