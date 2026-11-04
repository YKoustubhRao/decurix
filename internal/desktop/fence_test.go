package desktop

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The one host rule (the Windows app's WSL daemon runs with iptables off): forwarding is dropped only where source
// and destination are both in the pool desktop networks come from, so no desktop reaches another, and nothing else -
// another distro's Docker, a service published there, the way out - is touched. Added idempotently, taken out when
// the daemon ends and at uninstall, and in no script but the WSL host's dockerd.sh (resource plan R3, narrowed).
func TestTheDesktopFenceIsScopedToTheDesktopPool(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(raw), "\r\n", "\n")
	}
	dockerd := read("desktop/electron/wsl/dockerd.sh")
	setup := read("desktop/electron/wsl/setup.sh")

	pool := regexp.MustCompile(`(?m)^desktop_pool=(\S+)$`).FindStringSubmatch(dockerd)
	base := regexp.MustCompile(`"default-address-pools": \[\{ "base": "([^"]+)"`).FindStringSubmatch(setup)
	if pool == nil || base == nil || pool[1] != base[1] {
		t.Fatalf("dockerd.sh's desktop_pool %v is not daemon.json's pool %v", pool, base)
	}
	insert := `iptables -I FORWARD 1 -s "$desktop_pool" -d "$desktop_pool" -j DROP`
	check := `iptables -C FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP 2>/dev/null ||`
	if !strings.Contains(dockerd, check+"\n        "+insert) {
		t.Error("the fence is not added idempotently (-C before -I) with source and destination both in the pool")
	}
	if regexp.MustCompile(`iptables -I FORWARD[^\n]*-i br-\+`).MatchString(dockerd) {
		t.Error("a bridge-wide DROP is still added")
	}
	if !strings.Contains(dockerd, `iptables -D FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP`) ||
		!regexp.MustCompile(`wait "\$daemon"\nstatus=\$\?\n(?:#[^\n]*\n)*unfence\n`).MatchString(dockerd) {
		t.Error("the fence is not taken out when the daemon ends")
	}
	if !strings.Contains(read("desktop/electron/installer.nsh"), "/opt/decurix/dockerd.sh unfence") {
		t.Error("the uninstaller does not take the fence out")
	}

	drop := regexp.MustCompile(`iptables[^\n]*FORWARD[^\n]*-j DROP`)
	for _, dir := range []string{"desktop", "docker", "scripts", "agent", "internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() && (d.Name() == "node_modules" || d.Name() == "build") {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || strings.HasSuffix(p, "_test.go") || filepath.Base(p) == "dockerd.sh" {
				return nil
			}
			if raw, err := os.ReadFile(p); err == nil && drop.Match(raw) {
				t.Errorf("%s adds a FORWARD drop: the fence belongs to the WSL host's dockerd.sh alone", p)
			}
			return nil
		})
	}
}
