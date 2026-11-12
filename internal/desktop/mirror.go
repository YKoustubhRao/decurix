// Mirror entries of the host-app list: a browser the person has open on their own desktop, shown and driven from a VM.
package desktop

import (
	"strings"
	"unicode"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// mirrorBrowsers are the browsers a "live on this PC" entry is offered for: a word of the app's name, the program a
// running one is (what the Windows helper finds its window by), and its name as the list shows it. Words are whole, so
// "chrome" never matches "chromium".
var mirrorBrowsers = []struct{ word, program, name string }{
	{"chrome", "chrome", "Google Chrome"},
	{"chromium", "chromium", "Chromium"},
	{"edge", "msedge", "Microsoft Edge"},
	{"brave", "brave", "Brave"},
	{"vivaldi", "vivaldi", "Vivaldi"},
	{"opera", "opera", "Opera"},
}

// mirrorDescription is what the list says of a mirror entry: what it is, and what it costs the person.
const mirrorDescription = "The window you already have open on this computer, exactly as it is - signed in, with your tabs - " +
	"shown and used from the VM. Nothing is copied or started. It has to stay open and not minimised, and the worker acts as " +
	"you in it, so anything that can steer the worker can use your sign-ins."

// nameWords is the lower-case words of a name: letters and digits, split on everything else, so "Microsoft Edge" and
// "Koustubh (Your Chrome) - Chrome" hold "edge" and "chrome" but "Knowledge" holds neither.
func nameWords(name string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words[w] = true
	}
	return words
}

// mirrorApps are the "live on this PC" entries for the browsers among items (one per browser program, whichever of its
// shortcuts were found): a browser that cannot run on a private desktop can still be mirrored, but one that was not found
// at all is not offered. They carry their mode already, so they are not checked with the host (nothing is started).
func mirrorApps(items []HostAppItem) []HostAppItem {
	var out []HostAppItem
	seen := map[string]bool{}
	for _, item := range items {
		if item.AppType != "windows_app" || item.HostCmd == "" {
			continue
		}
		words := nameWords(item.Name)
		for _, b := range mirrorBrowsers {
			if !words[b.word] || seen[b.program] {
				continue
			}
			seen[b.program] = true
			out = append(out, HostAppItem{
				ID:          "mirror_" + b.program,
				Name:        b.name + " (live on this PC)",
				Category:    "Live on this PC",
				Description: mirrorDescription,
				Detected:    true,
				AppType:     "windows_app",
				Mode:        ModeHostGUI,
				HostCmd:     `cmd.exe /c start "" ` + winhost.MirrorTarget(b.program),
				Icon:        item.Icon,
			})
		}
	}
	return out
}
