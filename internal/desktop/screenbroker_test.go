package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readRepoFile is a file of the repository by its path from the root, with its line ends as LF.
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, strings.Split(path, "/")...)...))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// countScreenUserEnv is how many times env sets ScreenUserEnv.
func countScreenUserEnv(env []string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, ScreenUserEnv+"=") {
			n++
		}
	}
	return n
}

// TestScreenBrokerIsOnByDefault checks owner decision O-1 (2026-11-08): with DECURIX_SCREEN_BROKER unset or empty on
// the control plane, a new desktop is made in broker mode (DECURIX_SCREEN_USER=1, once).
func TestScreenBrokerIsOnByDefault(t *testing.T) {
	for _, value := range []string{"", "  "} {
		t.Setenv(ScreenBrokerEnv, value)
		env := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{})
		if countScreenUserEnv(env) != 1 || envValue(env, ScreenUserEnv) != "1" {
			t.Fatalf("with %s=%q a desktop's %s is %q (%d times), want \"1\" once: broker mode is the default",
				ScreenBrokerEnv, value, ScreenUserEnv, envValue(env, ScreenUserEnv), countScreenUserEnv(env))
		}
	}
	os.Unsetenv(ScreenBrokerEnv) // t.Setenv above restores the old value at the end
	if !screenBrokerOn() {
		t.Fatalf("with %s unset broker mode is off; it is the default", ScreenBrokerEnv)
	}
}

// TestScreenBrokerSwitchSetsTheScreenUserEnv checks both ways of the creation switch: with DECURIX_SCREEN_BROKER=0 (or
// false/off/no) on the control plane a new desktop's environment has no DECURIX_SCREEN_USER at all - the default
// mode's environment byte for byte - and with "1" (or anything else) it gets DECURIX_SCREEN_USER=1, once, and nothing
// else changes.
func TestScreenBrokerSwitchSetsTheScreenUserEnv(t *testing.T) {
	t.Setenv(ScreenBrokerEnv, "0")
	off := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{})
	if countScreenUserEnv(off) != 0 {
		t.Fatalf("with %s=0 a desktop got %s: %v", ScreenBrokerEnv, ScreenUserEnv, off)
	}
	for _, value := range []string{"false", "OFF", " no "} {
		t.Setenv(ScreenBrokerEnv, value)
		if env := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{}); countScreenUserEnv(env) != 0 {
			t.Fatalf("%s=%q left broker mode on; it says off", ScreenBrokerEnv, value)
		}
	}
	for _, value := range []string{"true", "yes", "2"} {
		t.Setenv(ScreenBrokerEnv, value)
		if env := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{}); countScreenUserEnv(env) != 1 {
			t.Fatalf("%s=%q turned broker mode off; only 0/false/off/no do", ScreenBrokerEnv, value)
		}
	}

	t.Setenv(ScreenBrokerEnv, "1")
	on := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{})
	if countScreenUserEnv(on) != 1 || envValue(on, ScreenUserEnv) != "1" {
		t.Fatalf("with %s=1 a desktop's %s is %q (%d times), want \"1\" once", ScreenBrokerEnv, ScreenUserEnv,
			envValue(on, ScreenUserEnv), countScreenUserEnv(on))
	}
	// Nothing else changes: the same environment plus the one variable.
	if len(on) != len(off)+1 {
		t.Fatalf("broker mode changed more than one variable: %d -> %d", len(off), len(on))
	}
	rest := make([]string, 0, len(on))
	for _, kv := range on {
		if !strings.HasPrefix(kv, ScreenUserEnv+"=") {
			rest = append(rest, kv)
		}
	}
	if strings.Join(rest, "\n") != strings.Join(off, "\n") {
		t.Fatal("broker mode changed the rest of the desktop's environment")
	}
}

// TestScreenBrokerImageHasTheScreenUser checks that the desktop image makes the screen user the switch names, as a
// system account with no login shell, and that the entrypoint makes its shared folder only in broker mode.
func TestScreenBrokerImageHasTheScreenUser(t *testing.T) {
	dockerfile := readRepoFile(t, "docker/desktop.basic.Dockerfile")
	if !strings.Contains(dockerfile, "useradd --system --no-create-home --shell /usr/sbin/nologin "+ScreenUser+" ") {
		t.Fatalf("the desktop image does not make %s as a system user with no login shell", ScreenUser)
	}
	entrypoint := readRepoFile(t, "agent/decurix-entrypoint.sh")
	guard := `if [ "${` + ScreenUserEnv + `:-0}" = "1" ] && /usr/bin/getent passwd ` + ScreenUser + ` >/dev/null 2>&1; then`
	start := strings.Index(entrypoint, guard)
	if start < 0 {
		t.Fatal("the entrypoint has no broker-mode block")
	}
	end := strings.Index(entrypoint[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("the entrypoint's broker-mode block does not end")
	}
	block := entrypoint[start : start+end]
	if !strings.Contains(block, "/usr/bin/install -d -m 711 -o "+ScreenUser+" -g "+ScreenUser+" /run/decurix-screen") {
		t.Fatal("the entrypoint does not make /run/decurix-screen (the screen user's, 0711) in broker mode")
	}
	if strings.Count(entrypoint, "/run/decurix-screen") != strings.Count(block, "/run/decurix-screen") {
		t.Fatal("the entrypoint names /run/decurix-screen outside its broker-mode block")
	}
	if !strings.Contains(block, "exec /usr/bin/setpriv") || strings.Contains(block, "/dockerstartup/startup.sh") {
		t.Fatal("the broker-mode block must end in its own exec and never run the default mode's startup")
	}
}
