package desktop

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A desktop starts no websockify (L3, docs/lightweight.md): the entrypoint hands the base image's startup
// --skip-novnc before either of its two ways into it, unless the desktop was created with DECURIX_DESKTOP_NOVNC=1.
func TestTheDesktopStartsNoWebsockifyUnlessAskedTo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "agent", "decurix-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	gate := regexp.MustCompile(`(?m)^if \[ "\$\{DECURIX_DESKTOP_NOVNC:-0\}" != "1" \]; then\n\s+set -- --skip-novnc "\$@"\nfi$`).FindStringIndex(script)
	if gate == nil {
		t.Fatal("the entrypoint does not pass --skip-novnc unless DECURIX_DESKTOP_NOVNC=1")
	}
	starts := regexp.MustCompile(`/dockerstartup/startup\.sh "\$@"`).FindAllStringIndex(script, -1)
	if len(starts) != 2 {
		t.Fatalf("%d starts of the base startup, want 2 (as root and as the desktop user)", len(starts))
	}
	for _, s := range starts {
		if s[0] < gate[1] {
			t.Errorf("a start of the base startup at byte %d comes before --skip-novnc is added", s[0])
		}
	}
}

// The autostart entries the image switches off are the ones docker/desktop.basic.Dockerfile names and explains, and
// never one a person or the worker relies on: at-spi (`gui` reads the accessibility tree through it), the clipboard
// manager, the keyring (Chrome's cookie key), xdg-user-dirs, the agent itself.
func TestTheImageSwitchesOffOnlyUnusedSessionServices(t *testing.T) {
	off, err := os.ReadFile(filepath.Join("..", "..", "agent", "decurix-autostart-off.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^Hidden=true$`).Match(off) || !strings.HasPrefix(string(off), "[Desktop Entry]\n") {
		t.Fatalf("decurix-autostart-off.desktop is not a Hidden=true desktop entry:\n%s", off)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docker", "desktop.basic.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	loop := regexp.MustCompile(`RUN for name in ([^;]+); do \\\n\s+install -m 644 -o headless -g headless /tmp/decurix-autostart-off\.desktop "/home/headless/\.config/autostart/\$name\.desktop"`).FindStringSubmatch(string(raw))
	if loop == nil {
		t.Fatal("the Dockerfile does not install the autostart overrides")
	}
	names := strings.Fields(loop[1])
	if len(names) == 0 {
		t.Fatal("no autostart entries switched off")
	}
	for _, name := range names {
		for _, kept := range []string{"at-spi", "clipman", "clipboard", "keyring", "xdg-user-dirs", "decurix-agent", "xfce4-panel", "xfdesktop", "xfwm4", "xfsettingsd"} {
			if strings.Contains(name, kept) {
				t.Errorf("%s is switched off; it is needed (%s)", name, kept)
			}
		}
		if !strings.Contains(string(raw), "#   "+name) && !strings.Contains(string(raw), ", "+name) && !strings.Contains(string(raw), name+",") {
			t.Errorf("%s is switched off without a line saying why", name)
		}
	}
}
