package decurix

import (
	"bufio"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// TestTheBuildContextIsWhatTheImagesCopy checks embed.go's agent patterns both ways: every agent/ path a desktop
// Dockerfile COPYs is in DesktopBuildContext (TheBot's through its archive), and the agent's unit tests are not - they
// went into every binary (7.5 MB) until the review of 2026-11-02, and no image or running binary reads them.
func TestTheBuildContextIsWhatTheImagesCopy(t *testing.T) {
	for _, dockerfile := range []string{"docker/desktop.basic.Dockerfile", "docker/tools.Dockerfile"} {
		f, err := os.Open(dockerfile)
		if err != nil {
			t.Fatal(err)
		}
		lines := bufio.NewScanner(f)
		for lines.Scan() {
			fields := strings.Fields(lines.Text())
			if len(fields) < 3 || !strings.EqualFold(fields[0], "COPY") || strings.HasPrefix(fields[1], "--") {
				continue
			}
			for _, src := range fields[1 : len(fields)-1] {
				if !strings.HasPrefix(src, "agent/") || strings.HasPrefix(src, TheBotDir+"/") {
					continue // TheBot's files ride in its archive (embed_gen.go)
				}
				if _, err := fs.Stat(DesktopBuildContext, src); err != nil {
					t.Errorf("%s COPYs %s, which embed.go does not embed: %v", dockerfile, src, err)
				}
			}
		}
		f.Close()
		if err := lines.Err(); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := fs.Stat(DesktopBuildContext, "agent/tests"); err == nil {
		t.Error("agent/tests is embedded: the agent's unit tests go into every binary")
	}
	entries, err := fs.ReadDir(DesktopBuildContext, "agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			t.Errorf("agent/%s is embedded: a dot or underscore name at the top of agent/ is no source", e.Name())
		}
	}
	if _, err := fs.Stat(DesktopBuildContext, "agent/decurix_agent/computer/__main__.py"); err != nil {
		t.Errorf("the gui command's __main__.py is not embedded: %v", err)
	}
}
