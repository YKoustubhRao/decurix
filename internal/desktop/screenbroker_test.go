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

// TestScreenBrokerSwitchSetsTheScreenUserEnv checks both ways of the creation switch: with DECURIX_SCREEN_BROKER=1 on
// the control plane a new desktop gets DECURIX_SCREEN_USER=1, and without it (unset, empty, or anything but "1") the
// desktop's environment has no such variable at all - the default is byte-for-byte what it was.
func TestScreenBrokerSwitchSetsTheScreenUserEnv(t *testing.T) {
	count := func(env []string) int {
		n := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, ScreenUserEnv+"=") {
				n++
			}
		}
		return n
	}

	t.Setenv(ScreenBrokerEnv, "")
	off := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{})
	if count(off) != 0 {
		t.Fatalf("with the switch off a desktop got %s: %v", ScreenUserEnv, off)
	}
	for _, value := range []string{"0", "true", "yes", "2"} {
		t.Setenv(ScreenBrokerEnv, value)
		if env := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{}); count(env) != 0 {
			t.Fatalf("%s=%q turned broker mode on; only \"1\" does", ScreenBrokerEnv, value)
		}
	}

	t.Setenv(ScreenBrokerEnv, "1")
	on := buildDesktopEnv("box", HarnessTheBot, "", "", Brain{})
	if count(on) != 1 || envValue(on, ScreenUserEnv) != "1" {
		t.Fatalf("with %s=1 a desktop's %s is %q (%d times), want \"1\" once", ScreenBrokerEnv, ScreenUserEnv,
			envValue(on, ScreenUserEnv), count(on))
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
	guard := `if [ "${` + ScreenUserEnv + `:-0}" = "1" ] && /usr/bin/getent passwd ` + ScreenUser + ` >/dev/null 2>&1; then
    /usr/bin/install -d -m 711 -o ` + ScreenUser + ` -g ` + ScreenUser + ` /run/decurix-screen
fi`
	if !strings.Contains(entrypoint, guard) {
		t.Fatal("the entrypoint does not make /run/decurix-screen (the screen user's, 0711) in broker mode only")
	}
	if strings.Count(entrypoint, "/run/decurix-screen") != strings.Count(guard, "/run/decurix-screen") {
		t.Fatal("the entrypoint names /run/decurix-screen outside its broker-mode block")
	}
}
