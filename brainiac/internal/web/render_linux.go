//go:build linux

package web

import (
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"brainiac/internal/isolate"
)

var noLandlockOnce sync.Once

// confineBrowser starts the browser under its Landlock file policy: through
// Brainiac's own sandbox helper (isolate.Wrap), which confines itself and
// then becomes the browser, so the browser and every process it starts keep
// the policy. Its home, temporary and cache folders are moved into the
// profile, the one place it may write. A kernel without Landlock keeps the
// old behaviour (the browser's files unconfined), and says so once in the
// log; on an appliance Render is off by default (web_render: false).
func confineBrowser(cmd *exec.Cmd, p isolate.Policy) error {
	if !isolate.CanSandbox() {
		noLandlockOnce.Do(func() {
			log.Printf("web: this kernel offers no Landlock, so the headless browser runs without a file policy (it can read what this user can)")
		})
		return nil
	}
	if len(p.Write) > 0 {
		home := filepath.FromSlash(p.Write[0])
		cmd.Env = withEnv(cmd.Env, "HOME="+home, "TMPDIR="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			"XDG_CACHE_HOME="+filepath.Join(home, ".cache"))
	}
	return isolate.Wrap(cmd, p)
}

// withEnv is env with each NAME=value set, replacing what was there.
func withEnv(env []string, set ...string) []string {
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		keep := true
		for _, s := range set {
			if strings.HasPrefix(s, name+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}
