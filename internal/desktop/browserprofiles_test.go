// Tests for browserprofiles.go: every profile of a browser is an app, and a profile's shortcut is not listed twice.
package desktop

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// fakeChrome lays out a Chrome install and a user's Local State on a drive, and returns the drive.
func fakeChrome(t *testing.T, localState string) string {
	t.Helper()
	drive := t.TempDir()
	exe := filepath.Join(drive, "Program Files", "Google", "Chrome", "Application", "chrome.exe")
	data := filepath.Join(drive, "Users", "pat", "AppData", "Local", "Google", "Chrome", "User Data")
	for _, d := range []string{filepath.Dir(exe), data} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(exe, []byte("test"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "Local State"), []byte(localState), 0644); err != nil {
		t.Fatal(err)
	}
	return drive
}

func TestEveryBrowserProfileIsAnAppOfItsOwn(t *testing.T) {
	drive := fakeChrome(t, `{"profile":{"profiles_order":["Default","Profile 3"],"info_cache":{
		"Profile 3":{"name":"Work","user_name":"pat@example.com"},
		"Default":{"name":"Your Chrome","user_name":"pat.home@example.com"},
		"Profile 9":{"name":"Temp","is_ephemeral":true},
		"Bad\"Dir":{"name":"Broken"}}}}`)
	t.Setenv("DECURIX_HOST_MNT_C", drive)

	seen, listed := map[string]bool{}, map[string]bool{}
	var chrome []HostAppItem
	for _, a := range scanBrowserProfiles(seen, listed) {
		if strings.HasPrefix(a.Name, "Google Chrome") {
			chrome = append(chrome, a)
		}
	}
	if len(chrome) != 2 {
		t.Fatalf("want the two usable profiles, in the browser's order; got %+v", chrome)
	}
	if chrome[0].Name != "Google Chrome - Your Chrome (pat.home@example.com)" || chrome[1].Name != "Google Chrome - Work (pat@example.com)" {
		t.Errorf("names: %q, %q", chrome[0].Name, chrome[1].Name)
	}
	target, args, ok := windowsTarget(chrome[1].HostCmd)
	if !ok || !strings.HasSuffix(target, `Google\Chrome\Application\chrome.exe`) || args != `"--profile-directory=Profile 3"` {
		t.Errorf("Work runs %q %q (%v): want chrome.exe with its own profile folder as one argument", target, args, ok)
	}
	if !runsInPlace(chrome[1]) {
		t.Error("a browser profile must run in place, from the person's own profile")
	}
	if chrome[0].ID == chrome[1].ID || sanitizeAppSlug(chrome[0].Name) == sanitizeAppSlug(chrome[1].Name) {
		t.Error("two profiles must be two apps, with two host sessions")
	}
	// The browser's plain entry says less than its profiles do, and is left out.
	for _, a := range scanInstalledWindowsBrowsers(seen) {
		if a.Name == "Google Chrome" {
			t.Error("the plain Google Chrome entry is listed beside its profiles")
		}
	}
	if !listed["chrome"] {
		t.Error("chrome's profiles were listed, but its shortcuts are not told so")
	}
}

func TestABrowserWithNoProfileListKeepsItsPlainEntry(t *testing.T) {
	drive := fakeChrome(t, `not json`)
	t.Setenv("DECURIX_HOST_MNT_C", drive)
	seen, listed := map[string]bool{}, map[string]bool{}
	for _, a := range scanBrowserProfiles(seen, listed) {
		if strings.HasPrefix(a.Name, "Google Chrome") && strings.Contains(a.HostCmd, drive) {
			t.Errorf("profiles listed from an unreadable Local State: %+v", a)
		}
	}
	if seen["google chrome"] && !listed["chrome"] {
		t.Error("the plain entry was claimed with no profile listed in its place")
	}
}

// shortcutFile writes a minimal Shell Link (.lnk) pointing at target with args, as Windows writes one: link info with a local base
// path, and the arguments as Unicode string data.
func shortcutFile(t *testing.T, target, args string) string {
	t.Helper()
	hdr := make([]byte, 76)
	binary.LittleEndian.PutUint32(hdr[0:], 0x4C)
	binary.LittleEndian.PutUint32(hdr[0x14:], 0x2|0x20|0x80) // HasLinkInfo, HasArguments, IsUnicode
	base := append([]byte(target), 0)
	info := make([]byte, 28)
	binary.LittleEndian.PutUint32(info[4:], 0x1C)                  // header size
	binary.LittleEndian.PutUint32(info[8:], 1)                     // VolumeIDAndLocalBasePath
	binary.LittleEndian.PutUint32(info[16:], 28)                   // local base path offset
	binary.LittleEndian.PutUint32(info[24:], uint32(28+len(base))) // common path suffix offset
	info = append(append(info, base...), 0)
	binary.LittleEndian.PutUint32(info[0:], uint32(len(info)))
	u := utf16.Encode([]rune(args))
	str := make([]byte, 2+2*len(u))
	binary.LittleEndian.PutUint16(str, uint16(len(u)))
	for i, c := range u {
		binary.LittleEndian.PutUint16(str[2+2*i:], c)
	}
	p := filepath.Join(t.TempDir(), "x.lnk")
	if err := os.WriteFile(p, append(append(hdr, info...), str...), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAProfileShortcutIsNotListedTwice(t *testing.T) {
	chrome := `C:\Program Files\Google\Chrome\Application\chrome.exe`
	profile := shortcutFile(t, chrome, `--profile-directory="Default"`)
	target, args, ok := readShortcut(profile)
	if !ok || target != chrome || args != `--profile-directory="Default"` {
		t.Fatalf("readShortcut = %q %q %v", target, args, ok)
	}
	listed := map[string]bool{"chrome": true}
	if !isBrowserProfileShortcut(profile, listed) {
		t.Error("a shortcut to a listed profile is listed again")
	}
	if isBrowserProfileShortcut(profile, map[string]bool{"msedge": true}) {
		t.Error("a shortcut to a browser whose profiles were not listed was dropped")
	}
	// A web app installed from the browser is an app of its own.
	if isBrowserProfileShortcut(shortcutFile(t, chrome, `--profile-directory=Default --app-id=abcdefghijklmnop`), listed) {
		t.Error("a web app's shortcut was taken for a profile's")
	}
	if isBrowserProfileShortcut(shortcutFile(t, `C:\Tools\other.exe`, `--profile-directory=Default`), listed) {
		t.Error("a shortcut to another program was dropped")
	}
	junk := filepath.Join(t.TempDir(), "junk.lnk")
	_ = os.WriteFile(junk, []byte("not a shortcut"), 0644)
	if _, _, ok := readShortcut(junk); ok {
		t.Error("a file that is not a shortcut was read as one")
	}
}
