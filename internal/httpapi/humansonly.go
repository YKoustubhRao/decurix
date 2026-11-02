package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Some routes show or change what only a person should: a mailbox's senders
// and subjects, the rules that turn email into tasks. The primary listener
// has no login, and the agents in the desktops can reach it (they call back
// to it), so an agent - or a prompt injected into one by a web page or an
// email - could read someone's inbox headers or add a rule that feeds it
// more. humansOnly refuses those routes to any request from a container.
//
// A container is told apart by its address: inside one of the Docker
// networks desktops use, and not that network's gateway (desktop.Manager.
// ContainerNetworks). A person's browser reaches the control plane from the
// host - loopback when the binary runs on the host, the network's gateway
// when compose publishes the port - and never from a container address.
// Checked live on both. The one setup it cannot tell apart is the released
// binary on Docker Desktop (macOS/Windows), where a container's call to
// host.docker.internal arrives on the host's loopback like the browser's.

// containerNetsTTL is how long the Docker network ranges are cached.
const containerNetsTTL = time.Minute

type containerNets struct {
	mu       sync.Mutex
	subnets  []netip.Prefix
	gateways []netip.Addr
	fetched  time.Time
}

func (s *Server) containerNetworks() ([]netip.Prefix, []netip.Addr) {
	return s.containerNetworksFresh(containerNetsTTL)
}

// containerNetworksFresh is containerNetworks, asking Docker again when what it knows is older than maxAge.
func (s *Server) containerNetworksFresh(maxAge time.Duration) ([]netip.Prefix, []netip.Addr) {
	s.nets.mu.Lock()
	defer s.nets.mu.Unlock()

	if s.Desktops != nil && time.Since(s.nets.fetched) > maxAge {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		subnets, gateways := s.Desktops.ContainerNetworks(ctx)
		cancel()

		// A Docker hiccup keeps the last ranges rather than forgetting them.
		if len(subnets) > 0 {
			s.nets.subnets, s.nets.gateways = subnets, gateways
		}

		s.nets.fetched = time.Now()
	}

	return s.nets.subnets, s.nets.gateways
}

// containerNetsRecheck bounds how often an address in no known range makes the ranges be asked for again.
const containerNetsRecheck = 2 * time.Second

// fromContainer reports whether r came from a container rather than from the host. It fails closed: an address
// that is not loopback, not a network's gateway and in no range known now makes the ranges be asked for again (a
// network made since, such as the practice one), and while no range is known at all, every such address is taken for
// a container (review 2026-11-02: an empty list made every container a person).
func (s *Server) fromContainer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return true
	}

	addr = addr.Unmap()
	if addr.IsLoopback() {
		return false
	}

	verdict := func(subnets []netip.Prefix, gateways []netip.Addr) (container, known bool) {
		for _, gw := range gateways {
			if addr == gw.Unmap() {
				return false, true
			}
		}
		for _, subnet := range subnets {
			if subnet.Contains(addr) {
				return true, true
			}
		}
		return false, false
	}

	subnets, gateways := s.containerNetworks()
	if container, known := verdict(subnets, gateways); known {
		return container
	}
	subnets, gateways = s.containerNetworksFresh(containerNetsRecheck)
	if container, known := verdict(subnets, gateways); known {
		return container
	}
	// No desktop manager means no containers (a test's server); with one, no known range means not known to be a person.
	return len(subnets) == 0 && s.Desktops != nil
}

// humansOnly serves h only to requests that do not come from a container.
func (s *Server) humansOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.fromContainer(r) {
			jsonStatusError(w, http.StatusForbidden, "not available from inside a desktop")
			return
		}

		h(w, r)
	}
}

// readsForAll serves reads (GET, HEAD) to anyone the listener lets in and every other method only to a person: a
// desktop's agent may read the org's skills and knowledge, never write them. Found in review (2026-10-27): with writes
// open to desktops, any worker could post an org-wide rule labelled as the person's, or install a skill into another
// worker.
func (s *Server) readsForAll(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && s.fromContainer(r) {
			jsonStatusError(w, http.StatusForbidden, "not available from inside a desktop")
			return
		}
		h.ServeHTTP(w, r)
	}
}
