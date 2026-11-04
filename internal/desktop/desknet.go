package desktop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Each desktop has a bridge network of its own, so no desktop reaches another's VNC (5901), screen (6901) or agent
// (8081): they share no network. The control plane's own container, and the bundled model server, join every
// desktop's network, so the control plane reaches each desktop and each desktop reaches the control plane (by its
// container name, which resolves on any network it is on) and the model server. Neither forwards between networks.
// No host firewall rule is needed, on WSL, a Mac's Docker Desktop or Linux alike (owner decision, resource plan R3).
//
// A desktop made before this is on the shared decurix-network and stays there, logged at each start as one to
// recreate to isolate: moving a running desktop's network was not tried without a live check, and a failed move
// would leave it unreachable. New desktops are on no network those share.

// desktopNetworkPrefix names a desktop's own network: decurix-desktop-<id>.
const desktopNetworkPrefix = "decurix-desktop-"

// desktopNetworkLabel marks a desktop's own network with the desktop's id.
const desktopNetworkLabel = "decurix.desktop-network"

// DesktopNetworkName is the name of desktop id's own bridge network.
func DesktopNetworkName(id string) string {
	return desktopNetworkPrefix + sanitizeName(id)
}

// ErrNoNetworkAddresses is Docker having no address range left for another desktop's own network.
var ErrNoNetworkAddresses = errors.New("Docker has no address range left for another desktop's own network")

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

// ensureDesktopNetwork makes desktop id's own network when it has none, and joins the control plane and the model
// server to it. It reports whether it made the network (so a failed creation removes only what it made).
func (m *Manager) ensureDesktopNetwork(ctx context.Context, id string) (bool, error) {
	name := DesktopNetworkName(id)
	created := false
	if _, err := m.client.NetworkInspect(ctx, name, network.InspectOptions{}); err != nil {
		if !client.IsErrNotFound(err) {
			return false, fmt.Errorf("check the desktop's network: %w", err)
		}
		if _, err := m.client.NetworkCreate(ctx, name, network.CreateOptions{
			Driver: "bridge",
			Labels: map[string]string{"decurix": "true", desktopNetworkLabel: sanitizeName(id)},
		}); err != nil {
			if text := strings.ToLower(err.Error()); strings.Contains(text, "non-overlapping") || strings.Contains(text, "address pool") ||
				strings.Contains(text, "no available") {
				return false, fmt.Errorf("%w: each desktop has a network of its own, one address range each. Delete a desktop you no longer need, or give Docker more ranges (default-address-pools in its daemon.json)", ErrNoNetworkAddresses)
			}
			return false, fmt.Errorf("create the desktop's network: %w", err)
		}
		created = true
	}
	if err := m.joinDesktopNetwork(ctx, name); err != nil {
		return created, err
	}
	return created, nil
}

// joinDesktopNetwork connects the control plane's own container (when it runs in one) and the model server (when
// there is one) to a desktop's network, each only once. The control plane must join: without it neither reaches the
// other. The model server is best effort: a desktop that uses another reaches that one.
func (m *Manager) joinDesktopNetwork(ctx context.Context, name string) error {
	if controlPlaneInContainer() {
		self, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("find the control plane's own container: %w", err)
		}
		if err := m.connectOnce(ctx, name, self); err != nil {
			return fmt.Errorf("connect the control plane to %s: %w", name, err)
		}
	}
	if m.ModelServer != "" {
		if err := m.connectOnce(ctx, name, m.ModelServer); err != nil && !client.IsErrNotFound(err) {
			log.Printf("desktop network %s: could not connect the model server %s: %v", name, m.ModelServer, err)
		}
	}
	return nil
}

// connectOnce connects container to network unless it is on it already.
func (m *Manager) connectOnce(ctx context.Context, networkName, containerName string) error {
	inspect, err := m.client.ContainerInspect(ctx, containerName)
	if err != nil {
		return err
	}
	if inspect.NetworkSettings != nil && inspect.NetworkSettings.Networks[networkName] != nil {
		return nil
	}
	return m.client.NetworkConnect(ctx, networkName, inspect.ID, nil)
}

// JoinDesktopNetworks joins the control plane and the model server to every desktop's own network: a control plane
// container made again (an update) or a model server started after the desktops is on none of them. Called at
// startup and once the model server is up.
func (m *Manager) JoinDesktopNetworks(ctx context.Context) {
	list, err := m.client.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", desktopNetworkLabel))})
	if err != nil {
		log.Printf("desktop networks: could not list them: %v", err)
		return
	}
	for _, n := range list {
		if err := m.joinDesktopNetwork(ctx, n.Name); err != nil {
			log.Printf("desktop networks: %v", err)
		}
	}
}

// removeDesktopNetwork removes desktop id's own network once its container is gone: the control plane and the model
// server leave it first. A network already gone is fine.
func (m *Manager) removeDesktopNetwork(ctx context.Context, id string) {
	name := DesktopNetworkName(id)
	if controlPlaneInContainer() {
		if self, err := os.Hostname(); err == nil {
			_ = m.client.NetworkDisconnect(ctx, name, self, true)
		}
	}
	if m.ModelServer != "" {
		_ = m.client.NetworkDisconnect(ctx, name, m.ModelServer, true)
	}
	if err := m.client.NetworkRemove(ctx, name); err != nil && !client.IsErrNotFound(err) {
		log.Printf("desktop %s: could not remove its network %s: %v", id, name, err)
	}
}

// logSharedNetwork says, for a desktop still on the shared network, that recreating it isolates it.
func logSharedNetwork(id string, networks map[string]*network.EndpointSettings) {
	if networks[DesktopNetworkName(id)] == nil && networks[NetworkName] != nil {
		log.Printf("desktop %s is on the shared %s, made before desktops had a network each: recreate it to isolate it from the other desktops", id, NetworkName)
	}
}
