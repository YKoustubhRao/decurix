// In-place browsers: a browser given to a VM runs from the person's own profile, shared by every VM.
package desktop

import (
	"strings"
	"unicode"
)

// ============================================================
// In place
//
// A browser copied into a desktop used to run on this computer in a private profile made from the person's, and nothing
// that profile needed in order to be signed in came across: on Chrome 127 and later the cookies and account tokens are
// encrypted ("v20", app-bound) so that only Chrome running from the profile's own folder can open them. Found live
// 2026-11-12 on Chrome 157: "Failed to decrypt token" for all three Google accounts on a copy made from the person's real
// files and started the ordinary way - and no error on the real folder. Every copy of a profile is therefore a signed-out
// browser, however it is made.
//
// So a browser is not copied, and it is not run anywhere of ours either: Chrome allows one process per profile folder, on one
// desktop, so a Chrome run out of sight on a private desktop (the first version of this) took the person's own Chrome from them -
// every launch of theirs was handed to the hidden process (found live 2026-11-13: "I can't open Chrome from my device"). The Windows
// helper asks the person's own running Chrome for ONE WINDOW (bridge.ps1, "A borrowed window"), keeps it invisible to them, and the VM
// shows and drives that: signed in as the person is, and their Chrome stays theirs to open and use as ever. Hence:
//   - every VM that opens it joins the same host session (packContext.Shared), each seeing the one window;
//   - it is never started ahead of a click, or kept ready: that would open a window in the person's Chrome, or start it, for nothing.
// ============================================================

// inPlaceDescription is what the app list says of a browser that runs in place.
const inPlaceDescription = "Your own browser, as it is: signed in as you, with your tabs and logins. The VM gets one window of the " +
	"Chrome on this computer, kept out of your sight, so your own Chrome stays yours to open and use as usual. Nothing is copied. Every " +
	"VM shares that one window. It acts as you, so anything that can steer the worker can use your logins."

// inPlaceBrowsers are the whole words in an app's name that make it a browser run in place (the same set as the helper's).
var inPlaceBrowsers = []string{"chrome", "chromium", "edge", "brave", "vivaldi", "opera"}

// nameWords is the lower-case words of a name: letters and digits, split on everything else, so "Microsoft Edge" and
// "Koustubh (Your Chrome) - Chrome" hold "edge" and "chrome" but "Knowledge" holds neither.
func nameWords(name string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words[w] = true
	}
	return words
}

// SlugRunsInPlace reports whether the app slug of a host session id (HostSessionSlug) names a browser run in place: what tells a
// browser that an older build kept ready - and saved to be started again at every start - from any other app.
func SlugRunsInPlace(slug string) bool {
	words := nameWords(slug)
	for _, b := range inPlaceBrowsers {
		if words[b] {
			return true
		}
	}
	return false
}

// runsInPlace reports whether an app of the host list is a browser the helper runs from the person's own profile.
func runsInPlace(app HostAppItem) bool {
	if app.AppType != "windows_app" {
		return false
	}
	words := nameWords(app.Name)
	for _, b := range inPlaceBrowsers {
		if words[b] {
			return true
		}
	}
	return false
}
