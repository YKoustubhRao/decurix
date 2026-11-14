// Browser profiles: every profile of each Chromium browser on this computer is an app of its own, named for its account.
package desktop

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
)

// ============================================================
// Browser profiles
//
// A Chromium browser keeps several profiles in one folder, each signed in to its own accounts, and a VM runs one of them in place
// (inplace.go): that profile's window, signed in as that profile is. Which profile the person meant is the whole question - and the
// app list used to know only what a shortcut file said: "Google Chrome" (no profile, so the browser picks one itself) and the shortcut
// Chrome puts on the desktop for one profile. Found live 2026-11-13: the person worked in a profile named "Koustubh"
// (koustubh.rao.dev) that had no shortcut, so the VM was given the "Your Chrome" profile - whose Google sign-in had lapsed on the
// person's own screen as well - and YouTube was signed out there. So each browser's own list of its profiles (its Local State) is read,
// and every profile is an app, named for the profile and its account; the browser's plain entry and its profile shortcuts, which
// say the same less clearly, are left out.
// ============================================================

// chromiumBrowser is a browser that keeps its profiles the Chromium way.
type chromiumBrowser struct {
	name     string   // as the app list names it
	exe      string   // its program's file name, lower case, without .exe
	installs []string // where its program is, relative to a drive ("*" is any user)
	userData string   // its profiles' folder, relative to a user's folder
}

var chromiumBrowsers = []chromiumBrowser{
	{"Google Chrome", "chrome", []string{
		"Program Files/Google/Chrome/Application/chrome.exe",
		"Program Files (x86)/Google/Chrome/Application/chrome.exe",
		"Users/*/AppData/Local/Google/Chrome/Application/chrome.exe",
	}, "AppData/Local/Google/Chrome/User Data"},
	{"Microsoft Edge", "msedge", []string{
		"Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
		"Program Files/Microsoft/Edge/Application/msedge.exe",
	}, "AppData/Local/Microsoft/Edge/User Data"},
	{"Brave", "brave", []string{
		"Program Files/BraveSoftware/Brave-Browser/Application/brave.exe",
		"Users/*/AppData/Local/BraveSoftware/Brave-Browser/Application/brave.exe",
	}, "AppData/Local/BraveSoftware/Brave-Browser/User Data"},
	{"Vivaldi", "vivaldi", []string{
		"Users/*/AppData/Local/Vivaldi/Application/vivaldi.exe",
		"Program Files/Vivaldi/Application/vivaldi.exe",
	}, "AppData/Local/Vivaldi/User Data"},
}

// browserProfile is one profile of a browser, as its Local State lists it.
type browserProfile struct {
	Dir     string // its folder in the user-data folder: "Default", "Profile 3"
	Name    string // what the person named it
	Account string // the account it is signed in to, if any
}

// readBrowserProfiles lists the profiles a Local State file names, in the browser's own order (the order of its profile menu).
func readBrowserProfiles(localState string) []browserProfile {
	raw, err := os.ReadFile(localState)
	if err != nil {
		return nil
	}
	var ls struct {
		Profile struct {
			InfoCache map[string]struct {
				Name        string `json:"name"`
				UserName    string `json:"user_name"`
				IsEphemeral bool   `json:"is_ephemeral"`
			} `json:"info_cache"`
			Order []string `json:"profiles_order"`
		} `json:"profile"`
	}
	if json.Unmarshal(raw, &ls) != nil {
		return nil
	}
	rank := map[string]int{}
	for i, d := range ls.Profile.Order {
		rank[d] = i + 1
	}
	var out []browserProfile
	for dir, p := range ls.Profile.InfoCache {
		// A folder name is passed to the browser as one argument; one that could break the command line is not offered.
		if dir == "" || p.IsEphemeral || strings.ContainsAny(dir, "\"\t\r\n\\/") {
			continue
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = dir
		}
		out = append(out, browserProfile{Dir: dir, Name: name, Account: strings.TrimSpace(p.UserName)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].Dir], rank[out[j].Dir]
		if ri == 0 || rj == 0 {
			if ri != rj {
				return ri != 0
			}
			return out[i].Dir < out[j].Dir
		}
		return ri < rj
	})
	return out
}

// newestLocalState is the Local State of the browser that was used last among the computer's users: the helper runs the browser
// as the person at the screen, whose own profiles those are.
func newestLocalState(drive, userData string) string {
	matches, _ := filepath.Glob(filepath.Join(drive, "Users", "*", filepath.FromSlash(userData), "Local State"))
	best, at := "", int64(0)
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && !fi.IsDir() && fi.ModTime().UnixNano() > at {
			best, at = m, fi.ModTime().UnixNano()
		}
	}
	return best
}

// browserProfileName is how the app list names a profile: the browser, the profile, and the account it is signed in to.
func browserProfileName(browser string, p browserProfile) string {
	name := browser + " - " + p.Name
	if p.Account != "" && !strings.EqualFold(p.Account, p.Name) {
		name += " (" + p.Account + ")"
	}
	return name
}

// scanBrowserProfiles lists every profile of each Chromium browser installed, as an app run in place with that profile. listed gets
// the program name of each browser whose profiles were listed, so its plain entry and its profile shortcuts are left out (they are
// claimed in seen, or skipped by scanWindowsStartMenu).
func scanBrowserProfiles(seen map[string]bool, listed map[string]bool) []HostAppItem {
	var items []HostAppItem
	for _, drive := range getWindowsCDriveCandidates() {
		for _, b := range chromiumBrowsers {
			if listed[b.exe] {
				continue
			}
			var exe string
			for _, pattern := range b.installs {
				matches, _ := filepath.Glob(filepath.Join(drive, filepath.FromSlash(pattern)))
				for _, m := range matches {
					if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
						exe = m
						break
					}
				}
				if exe != "" {
					break
				}
			}
			if exe == "" {
				continue
			}
			profiles := readBrowserProfiles(newestLocalState(drive, b.userData))
			if len(profiles) == 0 {
				continue // a browser never started, or one whose list cannot be read, keeps its plain entry
			}
			listed[b.exe] = true
			seen[strings.ToLower(b.name)], seen["win_"+sanitizeAppSlug(b.name)] = true, true
			for _, p := range profiles {
				name := browserProfileName(b.name, p)
				id := "win_" + sanitizeAppSlug(b.name+" "+p.Dir)
				if !claimApp(seen, name, id) {
					continue
				}
				items = append(items, HostAppItem{
					ID: id, Name: name, Category: "Browsers & Web",
					Description: "Windows browser profile", Detected: true,
					DetectedPaths: []string{exe}, AppType: "windows_app",
					HostCmd: fmt.Sprintf(`cmd.exe /c start "" "%s" --profile-directory="%s"`, wslToWindowsPath(exe), p.Dir),
					Icon:    pickAppIcon(b.name),
				})
			}
		}
	}
	return items
}

// isBrowserProfileShortcut reports whether a shortcut file opens a profile of one of the listed browsers - a browser started with
// --profile-directory and nothing else of note. A web app's shortcut (--app-id, --app) is an app of its own and is kept.
func isBrowserProfileShortcut(path string, listed map[string]bool) bool {
	if len(listed) == 0 || !strings.EqualFold(filepath.Ext(path), ".lnk") {
		return false
	}
	target, args, ok := readShortcut(path)
	if !ok || !strings.Contains(args, "--profile-directory") || strings.Contains(args, "--app") {
		return false
	}
	base := strings.ToLower(target)
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	return listed[strings.TrimSuffix(base, ".exe")]
}

// readShortcut reads a Windows shortcut (.lnk, the Shell Link format of MS-SHLLINK): the path it points at and its arguments.
// ok is false for a file that is not one, or one that points at something with no file path (a Store app, a folder by its id).
func readShortcut(path string) (target, args string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 76 || binary.LittleEndian.Uint32(b[0:4]) != 0x4C {
		return "", "", false
	}
	flags := binary.LittleEndian.Uint32(b[0x14:0x18])
	at := 76
	if flags&0x1 != 0 { // HasLinkTargetIDList
		if at+2 > len(b) {
			return "", "", false
		}
		at += 2 + int(binary.LittleEndian.Uint16(b[at:]))
	}
	if flags&0x2 != 0 { // HasLinkInfo
		if at+28 > len(b) {
			return "", "", false
		}
		info := b[at:]
		size := int(binary.LittleEndian.Uint32(info[0:4]))
		if size < 28 || size > len(info) {
			return "", "", false
		}
		info = info[:size]
		header := int(binary.LittleEndian.Uint32(info[4:8]))
		if binary.LittleEndian.Uint32(info[8:12])&0x1 != 0 { // VolumeIDAndLocalBasePath
			if header >= 0x24 && size >= 0x24 {
				target = utf16z(info, int(binary.LittleEndian.Uint32(info[28:32])))
			}
			if target == "" {
				target = cstring(info, int(binary.LittleEndian.Uint32(info[16:20])))
			}
		}
		at += size
	}
	unicode := flags&0x80 != 0
	for _, f := range []uint32{0x4, 0x8, 0x10, 0x20, 0x40} { // name, relative path, working dir, arguments, icon
		if flags&f == 0 {
			continue
		}
		if at+2 > len(b) {
			return "", "", false
		}
		n := int(binary.LittleEndian.Uint16(b[at:]))
		at += 2
		width := 1
		if unicode {
			width = 2
		}
		if at+n*width > len(b) {
			return "", "", false
		}
		var s string
		if unicode {
			u := make([]uint16, n)
			for i := range u {
				u[i] = binary.LittleEndian.Uint16(b[at+2*i:])
			}
			s = string(utf16.Decode(u))
		} else {
			s = string(b[at : at+n])
		}
		at += n * width
		if f == 0x20 {
			args = s
		}
	}
	return target, args, target != ""
}

// cstring is the NUL-terminated byte string at off in b ("" when off is outside b).
func cstring(b []byte, off int) string {
	if off <= 0 || off >= len(b) {
		return ""
	}
	end := off
	for end < len(b) && b[end] != 0 {
		end++
	}
	return string(b[off:end])
}

// utf16z is the NUL-terminated UTF-16 string at off in b ("" when off is outside b).
func utf16z(b []byte, off int) string {
	if off <= 0 || off+1 >= len(b) {
		return ""
	}
	var u []uint16
	for i := off; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
