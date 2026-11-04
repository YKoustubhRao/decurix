package desktop

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// On the Windows app's WSL host, Docker runs with iptables off and only dockerd.sh's fence keeps one desktop's
// network from another's (and its guard keeps a sibling distro's containers out). The control plane cannot see the
// distro's iptables from its container, so dockerd.sh writes what it put in place to /tmp/decurix-wsl/fence (the
// distro's /tmp is the control plane's, docker-compose.yaml), and the app says the control plane is on that host
// (DECURIX_HOST_KIND=wsl, main.js). There, a desktop is neither made nor started unless that file says the fence
// holds, and the ranges it names are the one source of truth for where desktop networks may be (review NET-R3-2).
// Elsewhere (Linux, a Mac's Docker Desktop) Docker's own per-bridge rules keep desktop networks apart.

// fenceStatusPath is where dockerd.sh says what it put in place; a variable so a test can write one.
var fenceStatusPath = "/tmp/decurix-wsl/fence"

// HostFenceState is what the WSL host's fence is, as dockerd.sh last said.
type HostFenceState struct {
	// Required is whether this host needs the fence (the Windows app's WSL host); nothing else below applies when not.
	Required bool `json:"required"`
	// Holds is whether the fence and the guard are both in place.
	Holds bool `json:"holds"`
	// State is dockerd.sh's word: ok, failed, off, or missing when it has said nothing.
	State string `json:"state"`
	// Pool is the range the fence covers, where every desktop network must be.
	Pool string `json:"pool,omitempty"`
}

// HostFence reads the WSL host's fence state; a host that does not need one has Required false.
func HostFence() HostFenceState {
	if strings.TrimSpace(os.Getenv("DECURIX_HOST_KIND")) != "wsl" {
		return HostFenceState{}
	}
	st := HostFenceState{Required: true, State: "missing"}
	f, err := os.Open(fenceStatusPath)
	if err != nil {
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch key {
		case "state":
			st.State = value
		case "pool":
			if p, err := netip.ParsePrefix(value); err == nil {
				st.Pool = p.Masked().String()
			}
		}
	}
	st.Holds = st.State == "ok" && st.Pool != ""
	return st
}

// ErrNoFence is a desktop refused on the WSL host while the fence between desktops is not in place.
var ErrNoFence = errors.New("the network fence that keeps desktops apart is not in place on this computer")

// ErrSubnetsOutsideFence is DECURIX_DESKTOP_SUBNETS naming a range the WSL host's fence does not cover.
var ErrSubnetsOutsideFence = errors.New("DECURIX_DESKTOP_SUBNETS is outside the range this computer's fence covers")

// requireFence is nil unless this host needs the fence and it does not hold.
func requireFence() error {
	st := HostFence()
	if !st.Required || st.Holds {
		return nil
	}
	return fmt.Errorf("%w (%s): restart Decurix; if it stays, the WSL host could not add its iptables rules", ErrNoFence, st.State)
}

// fencedSubnetRange is desktopSubnetRange, held to the WSL host's fence pool where there is one: a range outside it
// would get no fence, no guard and no NAT there.
func fencedSubnetRange() (netip.Prefix, error) {
	within := desktopSubnetRange()
	st := HostFence()
	if !st.Required {
		return within, nil
	}
	if err := requireFence(); err != nil {
		return within, err
	}
	pool, err := netip.ParsePrefix(st.Pool)
	if err != nil || !pool.Contains(within.Addr()) || within.Bits() < pool.Bits() {
		return within, fmt.Errorf("%w (%s is not inside %s)", ErrSubnetsOutsideFence, within, st.Pool)
	}
	return within, nil
}
