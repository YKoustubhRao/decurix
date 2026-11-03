package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// Some routes show or change what only a person should: a mailbox's senders and subjects, the rules that turn email
// into tasks, the access code, the backup. The primary listener has no login, and the agents in the desktops can
// reach it (they call back to it), so an agent - or a prompt injected into one by a web page or an email - could read
// someone's inbox headers or add a rule that feeds it more. humansOnly refuses those routes to anyone who is not a
// person.
//
// Who is a person is told by the address (desktop.NetView). On the primary listener it is the host and nothing else:
// loopback (a browser on this machine), or the gateway of a Docker network the control plane knows (a browser on the
// host through a port compose publishes, which Docker hands over from the control plane's own network's gateway).
// Every other address is not a person there: a desktop, the bundled Ollama, and a container on any network someone
// else made (review 2026-11-02, third round: a container on a user-made network read the access code, downloaded the
// backup, created a DAG and started a desktop, because an address in no known range counted as a person). A phone, a
// LAN laptop and the tunnel use the gated listener, whose login is the boundary there: it refuses only an address
// known to be a container. The one setup an address cannot tell apart is the released binary on Docker Desktop
// (macOS/Windows), where a container's call to host.docker.internal arrives on the host's loopback like the browser's.

// remoteAddr is r's source address, invalid when it cannot be read.
func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

// netView is what the control plane knows of the Docker networks (s.netsFor in tests).
func (s *Server) netView(ctx context.Context) desktop.NetView {
	if s.netsFor != nil {
		return s.netsFor()
	}
	return s.Desktops.Networks(ctx)
}

// origin is where r's source address puts it. Loopback is answered without asking anything; an address no network
// holds makes the networks be asked for again in the background (a network made since), never while r waits.
func (s *Server) origin(r *http.Request) desktop.Origin {
	addr := remoteAddr(r)
	if !addr.IsValid() {
		return desktop.OriginUnknown
	}
	if addr.IsLoopback() {
		return desktop.OriginHost
	}
	o := s.netView(r.Context()).Classify(addr)
	if o == desktop.OriginUnknown && s.netsFor == nil {
		s.Desktops.RecheckNetworks()
	}
	return o
}

// noNetworks reports whether this server has nothing to tell containers apart with: a test's server, with no desktop
// manager and no network view. Every caller is then the host.
func (s *Server) noNetworks() bool {
	return s.Desktops == nil && s.netsFor == nil
}

// fromContainer reports whether r must not be served as a person. On the primary listener that is anything but the
// host (loopback or a known gateway): unknown is not a person. On the gated listener, which has a login of its own,
// it is an address known to be a container's.
func (s *Server) fromContainer(r *http.Request) bool {
	if s.noNetworks() {
		return false
	}
	o := s.origin(r)
	if isGated(r) {
		return o == desktop.OriginContainer
	}
	return o != desktop.OriginHost
}

// fromKnownContainer reports whether r comes from an address inside a known Docker network (not its gateway).
func (s *Server) fromKnownContainer(r *http.Request) bool {
	return !s.noNetworks() && s.origin(r) == desktop.OriginContainer
}

// humansOnly serves h only to a person (fromContainer).
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
