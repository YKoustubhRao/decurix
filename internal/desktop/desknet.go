package desktop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Each desktop has a bridge network of its own, so no desktop reaches another's VNC (5901), screen (6901) or agent
// (8081): they share no network. The control plane's own container joins every desktop's network, so the control
// plane reaches each desktop and each desktop reaches the control plane (by its container name, which resolves on any
// network it is on). Nothing else joins them: a desktop reaches the bundled model through the control plane's
// allow-listed proxy (/api/native-ollama, buildDesktopEnv), not the model server's own API, so no shared container has
// a leg on every desktop network (review NET-R3 B3). The control plane does not forward between networks.
//
// The control plane joins a desktop network with the lowest gateway priority (GwPriority -1), and the networks are
// named to sort after decurix-network: Docker gives a container's default route, and the ports it publishes, to the
// endpoint of highest priority and, on a tie, the network whose name sorts first. A desktop network that won either
// moved the control plane's default route and its published 127.0.0.1 ports onto that desktop's bridge (review
// NET-R3 B2): the person's browser then came in from a desktop network's gateway and was refused.
//
// Each desktop network is a /28 of a Decurix range (DECURIX_DESKTOP_SUBNETS, 10.213.0.0/16 by default: the range the
// Windows app's daemon and its fence use), never one of Docker's default pools: those give about 30 networks per
// machine, shared with every compose project, and on a Mac's Docker Desktop one can shadow the person's own LAN (S1).
// A /28 overlapping any network Docker has or any route of the host (hostroutes.go) is skipped, and on the Windows
// app the range must be inside what dockerd.sh's fence covers (hostfence.go).
//
// A desktop made before this is on the shared decurix-network and stays there, said on its card (SharedNetwork) and at
// each start as one to recreate to isolate: moving a running desktop's network was not tried without a live check,
// and a failed move would leave it unreachable.

// desktopNetworkPrefix names a desktop's own network: decurix-vm-<id>, which sorts after decurix-network and
// decurix-practice (B2).
const desktopNetworkPrefix = "decurix-vm-"

// desktopNetworkLabel marks a desktop's own network with the desktop's id.
const desktopNetworkLabel = "decurix.desktop-network"

// desktopSubnetBits is the size of each desktop's own network: a /28 holds its gateway, the control plane and the
// desktop, with room to spare.
const desktopSubnetBits = 28

// defaultDesktopSubnets is the range desktop networks are carved from when DECURIX_DESKTOP_SUBNETS names none.
const defaultDesktopSubnets = "10.213.0.0/16"

// DesktopNetworkName is the name of desktop id's own bridge network.
func DesktopNetworkName(id string) string {
	return desktopNetworkPrefix + sanitizeName(id)
}

// desktopNetworkID is the desktop id a desktop network belongs to, and whether n is one: its name has the prefix and
// its label names the same desktop (S4: the label alone is anyone's to set).
func desktopNetworkID(n network.Summary) (string, bool) {
	id, ok := strings.CutPrefix(n.Name, desktopNetworkPrefix)
	return id, ok && id != "" && n.Labels[desktopNetworkLabel] == id
}

// ErrNoNetworkAddresses is no address range left for another desktop's own network.
var ErrNoNetworkAddresses = errors.New("no address range is left for another desktop's own network")

// desktopEndpoint is desktop id's endpoint on its own network, or, for a desktop made before desktops had one, on
// the shared decurix-network; nil when it has neither. Every lookup of a desktop's address goes through here
// (TestNoAddressIsReadFromTheSharedNetworkDirectly).
func desktopEndpoint(id string, networks map[string]*network.EndpointSettings) *network.EndpointSettings {
	if ep := networks[DesktopNetworkName(id)]; ep != nil && ep.IPAddress != "" {
		return ep
	}
	if ep := networks[NetworkName]; ep != nil && ep.IPAddress != "" {
		return ep
	}
	return nil
}

// desktopAddress is desktop id's IP address (desktopEndpoint), "" when it has none.
func desktopAddress(id string, networks map[string]*network.EndpointSettings) string {
	if ep := desktopEndpoint(id, networks); ep != nil {
		return ep.IPAddress
	}
	return ""
}

// onOwnNetwork reports whether desktop id is on a network of its own (one made since desktops have one).
func onOwnNetwork(id string, networks map[string]*network.EndpointSettings) bool {
	return networks[DesktopNetworkName(id)] != nil
}

// onSharedNetwork reports whether desktop id is still on the shared decurix-network alone: made before desktops had
// a network each, and so not isolated from the others until it is recreated.
func onSharedNetwork(id string, networks map[string]*network.EndpointSettings) bool {
	return !onOwnNetwork(id, networks) && networks[NetworkName] != nil
}

// desktopSubnetRange is the range desktop networks are carved from: DECURIX_DESKTOP_SUBNETS (an IPv4 prefix no
// smaller than a /28), else defaultDesktopSubnets.
func desktopSubnetRange() netip.Prefix {
	if raw := strings.TrimSpace(os.Getenv("DECURIX_DESKTOP_SUBNETS")); raw != "" {
		if p, err := netip.ParsePrefix(raw); err == nil && p.Addr().Is4() && p.Bits() <= desktopSubnetBits {
			return p.Masked()
		}
		log.Printf("DECURIX_DESKTOP_SUBNETS=%q is not an IPv4 range of a /28 or more; using %s", raw, defaultDesktopSubnets)
	}
	return netip.MustParsePrefix(defaultDesktopSubnets)
}

// freeDesktopSubnets is every /28 of within that overlaps none of taken, in order.
func freeDesktopSubnets(within netip.Prefix, taken []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	step := uint32(1) << (32 - desktopSubnetBits)
	base := within.Masked().Addr().As4()
	start := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	count := uint32(1) << (desktopSubnetBits - within.Bits())
	for i := uint32(0); i < count; i++ {
		a := start + i*step
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)}), desktopSubnetBits)
		free := true
		for _, t := range taken {
			if t.Overlaps(p) {
				free = false
				break
			}
		}
		if free {
			out = append(out, p)
		}
	}
	return out
}

// takenSubnets is the IPv4 range of every network the daemon has.
func (m *Manager) takenSubnets(ctx context.Context) ([]netip.Prefix, error) {
	list, err := m.client.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, n := range list {
		for _, cfg := range n.IPAM.Config {
			if p, err := netip.ParsePrefix(cfg.Subnet); err == nil && p.Addr().Is4() {
				out = append(out, p.Masked())
			}
		}
	}
	return out, nil
}

// joinsAsContainer and controlPlaneHost are whether the control plane runs in a container, which joins desktop
// networks, and that container's name; variables so a test can be the container.
var (
	joinsAsContainer = controlPlaneInContainer
	controlPlaneHost = os.Hostname
)

// desktopNetMu serialises choosing a desktop network's range, so two creations in one control plane never pick the
// same one (another daemon client could: Docker refuses the overlap, and the next range is tried).
var desktopNetMu sync.Mutex

// ensureDesktopNetwork makes desktop id's own network when it has none, and joins the control plane to it. It reports
// whether it made the network (so a failed creation removes only what it made).
func (m *Manager) ensureDesktopNetwork(ctx context.Context, id string) (bool, error) {
	name := DesktopNetworkName(id)
	created := false
	if _, err := m.client.NetworkInspect(ctx, name, network.InspectOptions{}); err != nil {
		if !client.IsErrNotFound(err) {
			return false, fmt.Errorf("check the desktop's network: %w", err)
		}
		if err := m.createDesktopNetwork(ctx, name, id); err != nil {
			return false, err
		}
		created = true
	}
	if err := m.joinDesktopNetwork(ctx, name); err != nil {
		return created, err
	}
	return created, nil
}

// createDesktopNetwork makes desktop id's network on the first free /28 of desktopSubnetRange.
func (m *Manager) createDesktopNetwork(ctx context.Context, name, id string) error {
	desktopNetMu.Lock()
	defer desktopNetMu.Unlock()
	within, err := fencedSubnetRange()
	if err != nil {
		return err
	}
	taken, err := m.takenSubnets(ctx)
	if err != nil {
		return fmt.Errorf("list the networks there are: %w", err)
	}
	// Nor any range the host itself routes (a VPN, a LAN): Docker checks none for an explicit subnet.
	taken = append(taken, hostRoutes(ctx)...)
	free := freeDesktopSubnets(within, taken)
	if len(free) == 0 {
		return fmt.Errorf("%w in %s: delete a desktop you no longer need, or give DECURIX_DESKTOP_SUBNETS a larger range", ErrNoNetworkAddresses, within)
	}
	var last error
	for i, subnet := range free {
		if i == 8 {
			break
		}
		_, err := m.client.NetworkCreate(ctx, name, network.CreateOptions{
			Driver: "bridge",
			IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet.String()}}},
			Labels: map[string]string{"decurix": "true", desktopNetworkLabel: sanitizeName(id)},
		})
		if err == nil {
			return nil
		}
		last = err
		// Taken since the list (another daemon client): the next one.
		if !strings.Contains(strings.ToLower(err.Error()), "overlap") {
			break
		}
	}
	return fmt.Errorf("create the desktop's network: %w", last)
}

// joinDesktopNetwork connects the control plane's own container (when it runs in one) to a desktop's network, once,
// with the lowest gateway priority (B2). Without it neither reaches the other, so a failure is an error.
func (m *Manager) joinDesktopNetwork(ctx context.Context, name string) error {
	if !joinsAsContainer() {
		return nil
	}
	self, err := controlPlaneHost()
	if err != nil {
		return fmt.Errorf("find the control plane's own container: %w", err)
	}
	inspect, err := m.client.ContainerInspect(ctx, self)
	if err != nil {
		return fmt.Errorf("connect the control plane to %s: %w", name, err)
	}
	if inspect.NetworkSettings != nil && inspect.NetworkSettings.Networks[name] != nil {
		return nil
	}
	if err := m.client.NetworkConnect(ctx, name, inspect.ID, desktopNetworkEndpoint()); err != nil {
		return fmt.Errorf("connect the control plane to %s: %w", name, err)
	}
	return nil
}

// desktopNetworkEndpoint is how the control plane joins a desktop's network: never its default route, never where its
// published ports go (B2).
func desktopNetworkEndpoint() *network.EndpointSettings {
	return &network.EndpointSettings{GwPriority: -1}
}

// JoinDesktopNetworks joins the control plane to every desktop's own network: a control plane container made again
// (an update) is on none of them. Called at startup; a network it cannot join is reported, and that desktop's next
// start joins it again or fails saying so.
func (m *Manager) JoinDesktopNetworks(ctx context.Context) error {
	list, err := m.client.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", desktopNetworkLabel))})
	if err != nil {
		return fmt.Errorf("list the desktop networks: %w", err)
	}
	var errs []error
	for _, n := range list {
		if _, ok := desktopNetworkID(n); !ok {
			continue
		}
		if err := m.joinDesktopNetwork(ctx, n.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SweepDesktopNetworks removes every desktop network whose desktop is gone (its container removed out of band, or a
// crash between making the network and the container) and that no creation in flight is making (S2). Called at
// startup, before JoinDesktopNetworks, so the control plane joins no orphan.
func (m *Manager) SweepDesktopNetworks(ctx context.Context) {
	list, err := m.client.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", desktopNetworkLabel))})
	if err != nil {
		log.Printf("desktop networks: could not list them to sweep: %v", err)
		return
	}
	for _, n := range list {
		id, ok := desktopNetworkID(n)
		if !ok {
			continue
		}
		if _, provisioning := m.GetProvisioning(id); provisioning {
			continue
		}
		if _, err := m.client.ContainerInspect(ctx, "desktop_"+id); err == nil || !client.IsErrNotFound(err) {
			continue
		}
		log.Printf("desktop networks: %s has no desktop; removing it", n.Name)
		m.removeDesktopNetwork(ctx, id)
	}
}

// removeDesktopNetwork removes desktop id's own network once its container is gone: the control plane leaves it
// first. A network already gone is fine. It is not cancelled with ctx: a half-done removal leaves the network.
func (m *Manager) removeDesktopNetwork(ctx context.Context, id string) {
	ctx = context.WithoutCancel(ctx)
	name := DesktopNetworkName(id)
	if joinsAsContainer() {
		if self, err := controlPlaneHost(); err == nil {
			_ = m.client.NetworkDisconnect(ctx, name, self, true)
		}
	}
	if err := m.client.NetworkRemove(ctx, name); err != nil && !client.IsErrNotFound(err) {
		log.Printf("desktop %s: could not remove its network %s: %v", id, name, err)
	}
}

// logSharedNetwork says, for a desktop still on the shared network, that recreating it isolates it.
func logSharedNetwork(id string, networks map[string]*network.EndpointSettings) {
	if onSharedNetwork(id, networks) {
		log.Printf("desktop %s is on the shared %s, made before desktops had a network each: recreate it to isolate it from the other desktops", id, NetworkName)
	}
}
