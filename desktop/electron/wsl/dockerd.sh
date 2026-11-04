#!/bin/sh
# The Windows app's Docker daemon, inside its own WSL distro. It stays in the
# foreground on purpose: WSL stops a distro once no wsl.exe is running in it,
# and the wsl.exe the app starts this with (detached, so it outlives the
# app's window) is what keeps the distro - and every desktop - running.
#
# When a daemon is already running (started by an earlier run of the app),
# this only waits on it, so there is always exactly one daemon.
set -u

# 10.212.0.0/24 is the daemon's default bridge, 10.213.0.0/16 the networks it
# makes (daemon.json); together 10.212.0.0/15.
ours=10.212.0.0/15
# The pool every network this daemon makes comes from (default-address-pools in setup.sh's daemon.json): each
# desktop's own network, the shared one and the practice one.
desktop_pool=10.213.0.0/16

# Each desktop has a bridge network of its own, so no desktop reaches another (internal/desktop/desknet.go). With
# "iptables": false Docker adds none of its own isolation between its networks, and the rules below forward the whole
# range, so traffic from one desktop network to another, routed through this distro, would go through. fence drops
# exactly that: traffic *routed* (not bridged) whose source and destination are both in desktop_pool.
#
# Routed, not bridged, matters (review NET-R3 B1): WSL2 distros share one network stack, and once another distro's
# Docker has run, br_netfilter is loaded with bridge-nf-call-iptables=1, so frames *within* one bridge pass FORWARD
# too - a desktop and the control plane on its network, the control plane and the model server on decurix-network.
# `-m physdev ! --physdev-is-bridged` leaves every such frame alone, br_netfilter on or off, so the fence never
# covers what stays inside one network (decurix-network itself included).
#
# Nothing else is touched: another distro's Docker (an ERPNext demo published from Ubuntu's Docker), its bridges, a
# desktop's way out, or a desktop reaching a service published elsewhere.
#
# guard (review NET-R3 P1) drops NEW connections forwarded into this daemon's range from outside it: a sibling
# distro's container routed to the control plane's address would otherwise arrive masqueraded as decurix-network's
# gateway, the address the control plane takes for the person on this computer. The person's own way in is a port
# published on 127.0.0.1, which docker-proxy relays locally (OUTPUT, never FORWARD), so it is not affected; replies
# to what desktops start are not NEW. In mangle's FORWARD, which Docker never rewrites or reorders.
#
# Both are idempotent and kept first in their chains (taken out and put back on every start, so ACCEPT rules added
# after them never sit above them); unfence takes them out when this daemon ends and when the app is uninstalled
# (installer.nsh).
fence_rule="-s $desktop_pool -d $desktop_pool -m physdev ! --physdev-is-bridged -j DROP"
guard_rule="! -s $ours -d $ours -m conntrack --ctstate NEW -j DROP"
drop_all() {
    # Every form this or an earlier build added: the bridge-wide drop, the unrouted pool drop, and the current two.
    while iptables -C FORWARD -i br-+ -o br-+ -j DROP 2>/dev/null; do iptables -D FORWARD -i br-+ -o br-+ -j DROP; done
    while iptables -C FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP 2>/dev/null; do
        iptables -D FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP
    done
    # shellcheck disable=SC2086
    while iptables -C FORWARD $fence_rule 2>/dev/null; do iptables -D FORWARD $fence_rule; done
    # shellcheck disable=SC2086
    while iptables -t mangle -C FORWARD $guard_rule 2>/dev/null; do iptables -t mangle -D FORWARD $guard_rule; done
}
# What is in place is written where the control plane reads it (the distro's /tmp is the control plane's /tmp,
# docker-compose.yaml): its state - ok only when both rules are there, checked after adding them - and the ranges it
# covers, the one source of truth for where desktop networks may be (desktop.HostFence). With anything but ok, the
# control plane refuses to make or start desktops on this host and says so (/api/docker/status): a missing fence is a
# gap between desktops, never only a line on stderr (review NET-R3-2).
fence_status=/tmp/decurix-wsl/fence
say_fence() {
    mkdir -p "${fence_status%/*}" && chmod 755 "${fence_status%/*}"
    printf 'state=%s\npool=%s\nours=%s\n' "$1" "$desktop_pool" "$ours" > "$fence_status.new" &&
        chmod 644 "$fence_status.new" && mv -f "$fence_status.new" "$fence_status"
}
fence() {
    drop_all
    # shellcheck disable=SC2086
    iptables -I FORWARD 1 $fence_rule || echo "dockerd.sh: the desktop fence could not be added" >&2
    # shellcheck disable=SC2086
    iptables -t mangle -I FORWARD 1 $guard_rule || echo "dockerd.sh: the forward guard could not be added" >&2
    # shellcheck disable=SC2086
    if iptables -C FORWARD $fence_rule 2>/dev/null && iptables -t mangle -C FORWARD $guard_rule 2>/dev/null; then
        say_fence ok
    else
        say_fence failed
    fi
}
unfence() {
    drop_all
    say_fence off
}
if [ "${1:-}" = unfence ]; then
    unfence
    exit 0
fi

# Fenced before anything else, a daemon kept running across an update included (review NET-R3 B4).
fence

running=$(pidof dockerd 2>/dev/null | awk '{print $1}')
if [ -n "$running" ]; then
    while kill -0 "$running" 2>/dev/null; do sleep 5; done
    exit 0
fi

# Docker's containers need shared mount propagation, which WSL's root is not.
mount --make-rshared / 2>/dev/null || true

if ! ip link show decurix0 >/dev/null 2>&1; then
    ip link add decurix0 type bridge
fi
ip addr flush dev decurix0
ip addr add 10.212.0.1/24 dev decurix0
ip link set decurix0 up

# Containers reach the internet through NAT, and forwarding is allowed for
# this range alone - inserted first, since another distro's Docker sets the
# FORWARD policy to DROP. Checked before each add, so a restart adds nothing twice.
sysctl -qw net.ipv4.ip_forward=1
iptables -t nat -C POSTROUTING -s "$ours" ! -d "$ours" -j MASQUERADE 2>/dev/null ||
    iptables -t nat -A POSTROUTING -s "$ours" ! -d "$ours" -j MASQUERADE
for side in -s -d; do
    iptables -C FORWARD "$side" "$ours" -j ACCEPT 2>/dev/null ||
        iptables -I FORWARD 1 "$side" "$ours" -j ACCEPT
done
# Fenced again, so the fence is above the ACCEPT rules just added (S5).
fence

rm -f /var/run/docker.pid

# The daemon's own log, kept to two files of at most 10 MB: it was appended to for good, and the distro has no
# logrotate (resource plan P1 #10). Copied and emptied in place, so dockerd keeps writing to the file it opened.
log=/var/log/dockerd.log
rotate() {
    size=$(wc -c < "$log" 2>/dev/null || echo 0)
    if [ "$size" -gt 10485760 ]; then
        cp "$log" "$log.1" && : > "$log"
    fi
}
rotate
dockerd >> "$log" 2>&1 &
daemon=$!
while kill -0 "$daemon" 2>/dev/null; do
    sleep 300
    rotate
done
wait "$daemon"
status=$?
# The stack stopped: its rule goes with it (the next start puts it back).
unfence
exit "$status"
