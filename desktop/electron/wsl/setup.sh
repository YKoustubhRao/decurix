#!/bin/sh
# Prepares the Windows app's own WSL distro ("Decurix", Alpine) to run the
# stack: Docker Engine, the daemon's settings, and the control plane's files.
# Run as root by the app (main.js, prepareDistro) before every start; each
# step is skipped when already done, so a second start takes a moment.
#
#   setup.sh STACK_DIR ENV_FILE
#
# STACK_DIR is the app's resources\stack as this distro sees it
# (/mnt/c/...): the Linux control plane, its Dockerfile, the compose file and
# VERSION. ENV_FILE is the person's .env from %APPDATA%\decurix.
set -eu

stack_src=$1
env_file=$2
here=$(cd "$(dirname "$0")" && pwd)
stack=/var/lib/decurix/stack

say() { echo "decurix-setup: $*"; }
# apk_safe: every apk call bounded and tried against each official mirror in turn (apk-safe.sh).
. "$here/apk-safe.sh"

# Docker Engine from Alpine's own packages. Only the first start downloads
# anything (about 70 MB). Each step is bounded and falls back to another
# mirror (apk_safe): the index in two minutes, the packages in twenty. A run
# killed half-way (the app's watchdog, a closed app) is safe to run again: the
# check below finds Docker missing and apk completes what it began.
if ! command -v dockerd >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
    say "installing Docker Engine"
    say "reading the package index"
    apk_safe 120 update
    say "downloading and installing Docker Engine (about 70 MB)"
    apk_safe 1200 add --no-progress docker docker-cli-compose iptables ip6tables bash
fi
# A Windows app shown in a desktop is started from here through the control
# plane's nsenter hop, which runs `bash -c` in this distro.
command -v bash >/dev/null 2>&1 || apk_safe 300 add --no-progress bash

mkdir -p /opt/decurix /etc/docker "$stack"
install -m 755 "$here/dockerd.sh" /opt/decurix/dockerd.sh

# WSL starts no init here: dockerd.sh is the one long-lived process, run by
# the app. The Windows PATH is left out of the distro's (it makes every
# command lookup cross into Windows, which is slow).
cat > /etc/wsl.conf <<'EOF'
[boot]
systemd=false
[interop]
enabled=true
appendWindowsPath=false
[user]
default=root
EOF

# Every WSL distro shares one network stack, so this daemon must not touch
# what another distro's Docker owns (a person's own Docker in Ubuntu uses
# docker0, 172.17/16 and the iptables chains named DOCKER*). It gets a bridge
# and an address range of its own, and dockerd.sh adds the two NAT rules it
# needs itself instead of letting Docker rewrite iptables.
#
# Images are kept in the classic overlay2 store, not Docker 29's default
# containerd store: the control plane builds the worker image through the
# classic builder (desktop.BuildImages), which commits every step, and with
# the containerd store each commit of a 3 GB image took minutes - three
# one-file COPY steps took 25 minutes on this repo's dev host.
cat > /etc/docker/daemon.json <<'EOF'
{
  "features": { "containerd-snapshotter": false },
  "storage-driver": "overlay2",
  "bridge": "decurix0",
  "iptables": false,
  "ip6tables": false,
  "ip-masq": false,
  "default-address-pools": [{ "base": "10.213.0.0/16", "size": 24 }],
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "2" }
}
EOF

# The control plane's files, copied only when the app is another build.
if [ "$(cat "$stack/VERSION" 2>/dev/null || true)" != "$(cat "$stack_src/VERSION")" ]; then
    say "staging $(cat "$stack_src/VERSION")"
    for name in decurix decurix-android-arm64 Dockerfile docker-compose.yaml; do
        cp -f "$stack_src/$name" "$stack/$name"
    done
    cp -f "$stack_src/VERSION" "$stack/VERSION"
fi
# The person's settings, every start (compose gives them to the control plane).
# They hold the person's keys: readable by root only, from the moment they exist.
(umask 077 && tr -d '\r' < "$env_file" > "$stack/.env")
chmod 600 "$stack/.env"
say "ready"
