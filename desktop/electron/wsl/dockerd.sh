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

# Each desktop has a network of its own, so no desktop reaches another (internal/desktop/desknet.go). With
# "iptables": false Docker adds none of its own isolation between its networks, and the rules below forward the whole
# range, so traffic from one desktop network to another, routed through this distro, would go through. fence drops
# exactly that: forwarding whose source and destination are both in desktop_pool. Nothing else is touched - another
# distro's Docker (WSL2 distros share one network stack: an ERPNext demo published from Ubuntu's Docker), its
# bridges, a desktop's way out, or a desktop reaching a service published elsewhere; and traffic within one network
# (a desktop and the control plane or the model server on it) is switched, never forwarded. Idempotent; unfence
# takes it out when this daemon ends and when the app is uninstalled (installer.nsh).
fence() {
    # The broader form of an earlier build, which dropped every bridge-to-bridge forward, goes first.
    while iptables -C FORWARD -i br-+ -o br-+ -j DROP 2>/dev/null; do iptables -D FORWARD -i br-+ -o br-+ -j DROP; done
    iptables -C FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP 2>/dev/null ||
        iptables -I FORWARD 1 -s "$desktop_pool" -d "$desktop_pool" -j DROP
}
unfence() {
    while iptables -C FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP 2>/dev/null; do
        iptables -D FORWARD -s "$desktop_pool" -d "$desktop_pool" -j DROP
    done
}
if [ "${1:-}" = unfence ]; then
    unfence
    exit 0
fi

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
# Desktops' networks kept apart (fence, above), first in FORWARD.
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
