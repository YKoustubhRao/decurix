package desktop

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/network"
)

// Who is calling the control plane is told by the address a request comes from, and only by that (review 2026-11-02,
// third round). The primary listener has no login, so a person there is the host itself and nothing else: loopback, or
// the gateway of a Docker network the control plane knows (a browser on the host reaching a port compose publishes
// arrives from the control plane's own network's gateway). An address inside any Docker network is a container. Any
// other address is unknown: not a person on the primary listener (a container on a network made by someone else - an
// ERPNext stack, a compose project - read the access code and the backup that way), while the gated listener, which
// has a login of its own, lets it try that login (a phone, a LAN laptop, the tunnel).

// Origin is where a request's source address puts it.
type Origin int

const (
	// OriginUnknown is an address in no Docker network the control plane knows of and not the host's.
	OriginUnknown Origin = iota
	// OriginHost is loopback or a gateway the host reaches its containers' networks through.
	OriginHost
	// OriginContainer is an address inside a Docker network that is not the host's way in.
	OriginContainer
)

// NetView is what the control plane knows of the Docker networks: every range a container can have an address in,
// and the gateways a request from the host itself arrives from.
type NetView struct {
	Subnets  []netip.Prefix
	Gateways []netip.Addr
}

// Classify says where addr puts a request: loopback and the known gateways are the host, an address in a known
// range is a container, and anything else is unknown.
func (v NetView) Classify(addr netip.Addr) Origin {
	if !addr.IsValid() {
		return OriginUnknown
	}
	addr = addr.Unmap()
	if addr.IsLoopback() {
		return OriginHost
	}
	for _, gw := range v.Gateways {
		if addr == gw.Unmap() {
			return OriginHost
		}
	}
	for _, subnet := range v.Subnets {
		if subnet.Contains(addr) {
			return OriginContainer
		}
	}
	return OriginUnknown
}

// merge is v with what o adds, each range and gateway once.
func (v NetView) merge(o NetView) NetView {
	out := NetView{Subnets: append([]netip.Prefix(nil), v.Subnets...), Gateways: append([]netip.Addr(nil), v.Gateways...)}
	for _, p := range o.Subnets {
		if !containsPrefix(out.Subnets, p) {
			out.Subnets = append(out.Subnets, p)
		}
	}
	for _, a := range o.Gateways {
		if !containsAddr(out.Gateways, a) {
			out.Gateways = append(out.Gateways, a)
		}
	}
	return out
}

// containsPrefix reports whether list holds p.
func containsPrefix(list []netip.Prefix, p netip.Prefix) bool {
	for _, q := range list {
		if q == p {
			return true
		}
	}
	return false
}

// containsAddr reports whether list holds a.
func containsAddr(list []netip.Addr, a netip.Addr) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

// How long what Docker said of its networks is used before it is asked again, and how soon an address no network
// holds may make it be asked again (a network made since).
const (
	netsTTL     = time.Minute
	netsRecheck = 2 * time.Second
	// dockerAskTimeout bounds one shared question to Docker.
	dockerAskTimeout = 3 * time.Second
)

// refreshed is a value fetched by one call at a time and shared: a reader takes the value there is and never queues
// behind Docker, except when it needs a newer one than there is, and then every such reader waits for the same fetch.
// Found in review (2026-11-02): container detection asked Docker under one lock, so every phone and LAN poll waited.
type refreshed[T any] struct {
	mu      sync.Mutex
	val     T
	at      time.Time     // when val was fetched; zero before the first fetch ends
	running chan struct{} // closed when the fetch in flight ends; nil while none runs
	gen     uint64        // what the value was fetched for (callers: the desktop list's generation)
}

// startLocked begins a fetch unless one is running, and returns the channel closed when it ends. c.mu is held. The
// fetch is detached from any caller: the answer is shared, so one caller giving up must not fail the others.
func (c *refreshed[T]) startLocked(gen uint64, fetch func(ctx context.Context, prev T) T) chan struct{} {
	if c.running != nil {
		return c.running
	}
	done := make(chan struct{})
	c.running = done
	prev := c.val
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dockerAskTimeout)
		v := fetch(ctx, prev)
		cancel()
		c.mu.Lock()
		c.val, c.at, c.gen, c.running = v, time.Now(), gen, nil
		c.mu.Unlock()
		close(done)
	}()
	return done
}

// current returns the value held, whether there is one yet, and starts a fetch in the background when it is older
// than maxAge. It never waits.
func (c *refreshed[T]) current(maxAge time.Duration, fetch func(context.Context, T) T) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= maxAge {
		c.startLocked(c.gen, fetch)
	}
	return c.val, !c.at.IsZero()
}

// fresh returns a value fetched less than within ago for generation gen, and true; when the one held is older (or
// of another generation) it waits, until ctx ends, for a fetch every caller needing one shares. It answers false when
// ctx ended first: the value it has then is not one to trust.
func (c *refreshed[T]) fresh(ctx context.Context, within time.Duration, gen uint64, fetch func(context.Context, T) T) (T, bool) {
	var zero T
	// Twice at most: a fetch already running may have begun for an older generation (a desktop made since).
	for range 2 {
		c.mu.Lock()
		if !c.at.IsZero() && time.Since(c.at) < within && c.gen >= gen {
			v := c.val
			c.mu.Unlock()
			return v, true
		}
		done := c.startLocked(gen, fetch)
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return zero, false
		}
		c.mu.Lock()
		if c.gen >= gen && !c.at.IsZero() {
			v := c.val
			c.mu.Unlock()
			return v, true
		}
		c.mu.Unlock()
	}
	return zero, false
}

// netCache is what Manager.Networks answers from. docker is the part Docker said, kept through a Docker hiccup.
type netCache struct {
	refreshed[netParts]
}

// netParts is a NetView with its two sources apart.
type netParts struct {
	docker NetView
	local  NetView
}

// Networks is what the control plane knows of the Docker networks now: what Docker said (every bridge network's
// ranges; decurix-network's and the default bridge's gateways) and what this machine's own interfaces say, which
// needs no Docker (the control plane's own network and its gateway in compose; the Docker bridges on a Linux host).
// It waits only for the very first answer; after that a stale view is answered at once and asked again in the
// background.
func (m *Manager) Networks(ctx context.Context) NetView {
	if m == nil {
		return localNetView()
	}
	parts, ok := m.callers.nets.current(netsTTL, m.fetchNetworks)
	if !ok {
		parts, _ = m.callers.nets.fresh(ctx, netsTTL, 0, m.fetchNetworks)
	}
	local := parts.local
	if len(parts.docker.Gateways) > 0 {
		// Docker named the one gateway the person comes in from (decurix-network's): the default route this container
		// has is not taken for another (review NET-R3 B2: a desktop network once held it).
		local.Gateways = nil
	}
	view := parts.docker.merge(local)
	if len(view.Subnets) == 0 && len(view.Gateways) == 0 {
		// The first answer is still coming (or ctx ended): what the interfaces say needs no Docker.
		return localNetView()
	}
	return view
}

// RecheckNetworks asks Docker again in the background, at most every netsRecheck: a request came from an address in
// no known network, which may be one made since.
func (m *Manager) RecheckNetworks() {
	if m == nil {
		return
	}
	m.callers.nets.current(netsRecheck, m.fetchNetworks)
}

// fetchNetworks asks Docker for its networks and reads this machine's interfaces. A Docker hiccup keeps what Docker
// said last rather than forgetting every range.
func (m *Manager) fetchNetworks(ctx context.Context, prev netParts) netParts {
	parts := netParts{docker: prev.docker, local: localNetView()}
	if m.callers.netsFetch != nil {
		if v, ok := m.callers.netsFetch(ctx); ok {
			parts.docker = v
		}
		return parts
	}
	if m.client == nil {
		return parts
	}
	if v, ok := m.dockerNetworks(ctx); ok {
		parts.docker = v
	}
	return parts
}

// dockerNetworks is every bridge network's ranges, and decurix-network's gateway: the one address the person's own
// browser comes in from, since the ports compose publishes on 127.0.0.1 are on decurix-network (the control plane
// joins every other network with the lowest gateway priority, desknet.go) and Docker's proxy hands them over from
// that network's gateway. No other gateway is the host's way in to the control plane: not the default bridge's, not
// a desktop network's, and never the practice network's (a practice desktop reaches only its broker). An address is
// a weak signal on its own - on the Windows app's WSL host a sibling distro's container, routed in, arrived
// masqueraded as decurix-network's gateway - so that host also drops every connection forwarded into Decurix's
// range from outside it (dockerd.sh's guard, review NET-R3 P1). macvlan and ipvlan networks are left out: they share
// the LAN's own range, where phones are.
func (m *Manager) dockerNetworks(ctx context.Context) (NetView, bool) {
	list, err := m.client.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return NetView{}, false
	}
	var v NetView
	for _, n := range list {
		if n.Driver != "bridge" {
			continue
		}
		for _, cfg := range n.IPAM.Config {
			if prefix, err := netip.ParsePrefix(cfg.Subnet); err == nil && !containsPrefix(v.Subnets, prefix.Masked()) {
				v.Subnets = append(v.Subnets, prefix.Masked())
			}
			if n.Name != NetworkName {
				continue
			}
			if gw, err := netip.ParseAddr(cfg.Gateway); err == nil && !containsAddr(v.Gateways, gw.Unmap()) {
				v.Gateways = append(v.Gateways, gw.Unmap())
			}
		}
	}
	return v, len(v.Subnets) > 0
}

// localNetView is what this machine's own interfaces say of the Docker networks, with no Docker to ask; a variable so
// tests can set it. Only Linux is read: on macOS and Windows Docker runs in a VM, and a container's call reaches the
// host on its loopback (the one setup an address cannot tell apart, docs/architecture.md).
var localNetView = func() NetView {
	if runtime.GOOS != "linux" {
		return NetView{}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return NetView{}
	}
	return interfaceNetView(ifaces, interfacePrefixes, controlPlaneInContainer(), defaultGateways)
}

// interfaceNetView reads the Docker networks off the interfaces. On a host, the Docker bridges (docker0, br-*) are
// container ranges. In a container that is on its own network (no Docker bridge among its interfaces - with host
// networking there would be), every interface's range is a Docker network and the default route's gateway is the
// host's way in: a port compose publishes is handed over from it.
func interfaceNetView(ifaces []net.Interface, prefixes func(net.Interface) []netip.Prefix, inContainer bool, gateways func() []netip.Addr) NetView {
	var bridges, others []net.Interface
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Name == "docker0" || strings.HasPrefix(iface.Name, "br-") {
			bridges = append(bridges, iface)
		} else {
			others = append(others, iface)
		}
	}
	var v NetView
	add := func(list []net.Interface) {
		for _, iface := range list {
			for _, p := range prefixes(iface) {
				if !containsPrefix(v.Subnets, p) {
					v.Subnets = append(v.Subnets, p)
				}
			}
		}
	}
	if !inContainer || len(bridges) > 0 {
		add(bridges)
		return v
	}
	add(others)
	for _, gw := range gateways() {
		for _, p := range v.Subnets {
			if p.Contains(gw) && !containsAddr(v.Gateways, gw) {
				v.Gateways = append(v.Gateways, gw)
			}
		}
	}
	return v
}

// interfacePrefixes is the ranges of iface's addresses, masked.
func interfacePrefixes(iface net.Interface) []netip.Prefix {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok || addr.Unmap().IsLinkLocalUnicast() {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		if p, err := addr.Unmap().Prefix(ones); err == nil && ones > 0 {
			out = append(out, p)
		}
	}
	return out
}

// defaultGateways is the IPv4 gateways of this machine's default routes (/proc/net/route).
func defaultGateways() []netip.Addr {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseDefaultGateways(f)
}

// parseDefaultGateways reads the gateways of the default routes from /proc/net/route's table: destination 00000000,
// the gateway in hex, in the machine's (little-endian) byte order.
func parseDefaultGateways(r interface{ Read([]byte) (int, error) }) []netip.Addr {
	var out []netip.Addr
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(f[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], binary.LittleEndian.Uint32(raw))
		if gw := netip.AddrFrom4(b); !gw.IsUnspecified() && !containsAddr(out, gw) {
			out = append(out, gw)
		}
	}
	return out
}
