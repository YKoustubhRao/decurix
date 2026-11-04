package desktop

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The WSL host's two rules (its daemon runs with iptables off). The fence drops traffic routed - never bridged -
// between two addresses of the pool desktop networks come from, so no desktop reaches another while every network
// still carries its own traffic with br_netfilter on (review NET-R3 B1: bridged frames pass FORWARD then). The guard
// drops NEW connections forwarded into the daemon's range from outside it (P1: a sibling distro's container came in
// masqueraded as decurix-network's gateway). Both: put first on every start, before the already-running check (B4)
// and again after the ACCEPT rules (S5), each earlier form taken out, taken out when the daemon ends and at
// uninstall, and in no script but the WSL host's dockerd.sh.
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
	for _, want := range []string{
		`fence_rule="-s $desktop_pool -d $desktop_pool -m physdev ! --physdev-is-bridged -j DROP"`,
		`guard_rule="! -s $ours -d $ours -m conntrack --ctstate NEW -j DROP"`,
		// Every form out first (so a restart adds nothing twice and each rule goes back on top), then each first.
		`while iptables -C FORWARD $fence_rule 2>/dev/null; do iptables -D FORWARD $fence_rule; done`,
		`while iptables -t mangle -C FORWARD $guard_rule 2>/dev/null; do iptables -t mangle -D FORWARD $guard_rule; done`,
		`while iptables -C FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP 2>/dev/null; do`,
		`while iptables -C FORWARD -i br-+ -o br-+ -j DROP 2>/dev/null; do`,
		`iptables -I FORWARD 1 $fence_rule`,
		`iptables -t mangle -I FORWARD 1 $guard_rule`,
	} {
		if !strings.Contains(dockerd, want) {
			t.Errorf("dockerd.sh lacks %s", want)
		}
	}
	// The only FORWARD drop added is the physdev fence: an unrouted pool-wide one cut every network's own traffic
	// with br_netfilter on, and a bridge-wide one did too.
	for _, add := range regexp.MustCompile(`iptables (?:-t \S+ )?-I FORWARD[^\n]*`).FindAllString(dockerd, -1) {
		if strings.Contains(add, "DROP") || !(strings.Contains(add, "$fence_rule") || strings.Contains(add, "$guard_rule") || strings.Contains(add, "-j ACCEPT")) {
			t.Errorf("dockerd.sh adds %q", add)
		}
	}
	// fence runs before the already-running check (B4), and again after the ACCEPT rules (S5).
	fenceAt := strings.Index(dockerd, "\nfence\n")
	runningAt := strings.Index(dockerd, "running=$(pidof dockerd")
	acceptAt := strings.Index(dockerd, `iptables -I FORWARD 1 "$side" "$ours" -j ACCEPT`)
	lastFence := strings.LastIndex(dockerd, "\nfence\n")
	if fenceAt < 0 || runningAt < 0 || fenceAt > runningAt {
		t.Error("the fence is not put in place before the already-running check")
	}
	if acceptAt < 0 || lastFence < acceptAt {
		t.Error("the fence is not put back above the ACCEPT rules")
	}
	// What is in place is said where the control plane reads it (desktop.HostFence): ok only when both rules are
	// checked present after adding them, failed otherwise, off once taken out (review NET-R3-2).
	for _, want := range []string{
		"fence_status=/tmp/decurix-wsl/fence",
		`printf 'state=%s\npool=%s\nours=%s\n' "$1" "$desktop_pool" "$ours"`,
		"if iptables -C FORWARD $fence_rule 2>/dev/null && iptables -t mangle -C FORWARD $guard_rule 2>/dev/null; then\n        say_fence ok\n    else\n        say_fence failed\n    fi",
	} {
		if !strings.Contains(dockerd, want) {
			t.Errorf("dockerd.sh does not say what is in place: lacks %q", want)
		}
	}
	if !regexp.MustCompile(`unfence\(\) \{\n    drop_all\n    say_fence off\n\}`).MatchString(dockerd) ||
		!regexp.MustCompile(`wait "\$daemon"\nstatus=\$\?\n(?:#[^\n]*\n)*unfence\n`).MatchString(dockerd) {
		t.Error("the rules are not taken out when the daemon ends")
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
