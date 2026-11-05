package desktop

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// With an explicit subnet per desktop network, Docker no longer checks a network's range against the host's own
// routes, as it does when it picks one from its pools. A desktop network on a range the host routes elsewhere (a VPN,
// a LAN in 10.213/16) would shadow it for that desktop and for the control plane joined to it. So every route of
// the host - read from its own /proc/net/route, through a hop when the control plane runs in a container - is taken
// as a range no desktop network may overlap (review NET-R3-2). Docker's own bridges are left out (their networks are
// listed by Docker), and so is the default route. What a Docker Desktop VM sees is the VM's routes, not the Mac's or
// Windows' own: there the operator sets DECURIX_DESKTOP_SUBNETS clear of a VPN (docs/running.md).

// hostRoutesTTL is how long the host's routes, once read, are used.
const hostRoutesTTL = 10 * time.Minute

var hostRouteCache struct {
	mu     sync.Mutex
	routes []netip.Prefix
	at     time.Time
}

// readHostRouteTable is the host's /proc/net/route: its own when the control plane runs on the host, else through a
// hop into the host's namespaces. A variable so a test can stand one in.
var readHostRouteTable = func(ctx context.Context) (io.Reader, error) {
	if !controlPlaneInContainer() {
		f, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return nil, err
		}
		return strings.NewReader(string(f)), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := HopCommand(ctx, findDockerBinary(), "routes", false, "cat /proc/net/route").Output()
	if err != nil {
		return nil, err
	}
	return strings.NewReader(string(out)), nil
}

// hostRoutes is the host's routed ranges (hostRouteCache), read again after hostRoutesTTL. A host whose routes
// cannot be read gives none, said in the log: the check guards against shadowing a route, not a boundary.
func hostRoutes(ctx context.Context) []netip.Prefix {
	hostRouteCache.mu.Lock()
	defer hostRouteCache.mu.Unlock()
	if !hostRouteCache.at.IsZero() && time.Since(hostRouteCache.at) < hostRoutesTTL {
		return hostRouteCache.routes
	}
	table, err := readHostRouteTable(ctx)
	if err != nil {
		log.Printf("desktop networks: the host's routes could not be read (%v); desktop subnets are checked against Docker's networks only", err)
		hostRouteCache.at = time.Now()
		hostRouteCache.routes = nil
		return nil
	}
	hostRouteCache.routes, hostRouteCache.at = parseHostRoutes(table), time.Now()
	return hostRouteCache.routes
}

// parseHostRoutes reads /proc/net/route's table: every route's destination and mask (little-endian hex), but the
// default route and Docker's own bridges and their veths.
func parseHostRoutes(r io.Reader) []netip.Prefix {
	var out []netip.Prefix
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[0] == "Iface" {
			continue
		}
		iface := f[0]
		if iface == "docker0" || iface == "decurix0" || strings.HasPrefix(iface, "br-") || strings.HasPrefix(iface, "veth") {
			continue
		}
		dest, mask := hexAddr(f[1]), hexAddr(f[7])
		if !dest.IsValid() || !mask.IsValid() || dest.IsUnspecified() {
			continue
		}
		m := mask.As4()
		bits := 0
		for _, b := range m {
			for i := 7; i >= 0 && b&(1<<i) != 0; i-- {
				bits++
			}
		}
		if p, err := dest.Prefix(bits); err == nil && bits > 0 && !containsPrefix(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// hexAddr is an IPv4 address written as /proc/net/route writes it: hex, in the machine's (little-endian) order.
func hexAddr(s string) netip.Addr {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 4 {
		return netip.Addr{}
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], binary.LittleEndian.Uint32(raw))
	return netip.AddrFrom4(b)
}
