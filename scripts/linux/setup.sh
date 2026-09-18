#!/bin/bash

set -e

echo "========================================"
echo "        Flowera Setup"
echo "========================================"

if [ "$EUID" -ne 0 ]; then
    echo "Please run:"
    echo "  sudo ./scripts/linux/setup.sh"
    exit 1
fi

REAL_USER="${SUDO_USER:-$USER}"

echo ""
echo "=== Updating apt ==="

apt-get update

echo ""
echo "=== Installing base dependencies ==="

apt-get install -y \
    ca-certificates \
    curl \
    git \
    qemu-system-x86 \
    qemu-utils \
    jq \
    unzip \
    openssh-client

echo ""
echo "=== Installing Docker ==="

if command -v docker >/dev/null 2>&1; then
    echo "Docker already installed."
else
    echo "Installing Docker..."

    install -m 0755 -d /etc/apt/keyrings

    curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
        -o /etc/apt/keyrings/docker.asc

    chmod a+r /etc/apt/keyrings/docker.asc

    . /etc/os-release

    echo \
      "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
      ${VERSION_CODENAME} stable" \
      > /etc/apt/sources.list.d/docker.list

    apt-get update

    apt-get install -y \
        docker-ce \
        docker-ce-cli \
        containerd.io \
        docker-buildx-plugin \
        docker-compose-plugin
fi

echo ""
echo "=== Enabling Docker ==="

systemctl enable docker
systemctl start docker

echo ""
echo "=== Configuring Docker for ${REAL_USER} ==="

if id "$REAL_USER" >/dev/null 2>&1; then
    usermod -aG docker "$REAL_USER"
else
    echo "WARNING: Could not find user ${REAL_USER}"
fi

echo ""
echo "=== Installing Cloudflare cloudflared ==="

if command -v cloudflared >/dev/null 2>&1; then
    echo "cloudflared already installed."
else
    install -m 0755 -d /usr/share/keyrings

    curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg \
        -o /usr/share/keyrings/cloudflare-main.gpg

    echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" \
        > /etc/apt/sources.list.d/cloudflared.list

    apt-get update

    apt-get install -y cloudflared
fi

echo ""
echo "========================================"
echo "        Verifying Installation"
echo "========================================"

echo ""
echo "Git:"
git --version

echo ""
echo "Docker:"
docker --version

echo ""
echo "Docker Compose:"
docker compose version

echo ""
echo "QEMU:"
qemu-system-x86_64 --version | head -n 1

echo ""
echo "Cloudflared:"
cloudflared --version

if [ -d /sys/kernel/mm/ksm ]; then
    echo ""
    echo "=== Enabling Linux Kernel Samepage Merging (KSM) for RAM deduplication ==="
    echo 1 > /sys/kernel/mm/ksm/run 2>/dev/null && echo "KSM enabled (run=1)" || true
    echo 1 > /sys/kernel/mm/ksm/use_zero_pages 2>/dev/null && echo "KSM zero pages deduplication enabled" || true
    echo 1000 > /sys/kernel/mm/ksm/pages_to_scan 2>/dev/null || true
    echo 20 > /sys/kernel/mm/ksm/sleep_millisecs 2>/dev/null || true
fi


echo ""
echo "========================================"
echo "        Setup Complete"
echo "========================================"

echo ""
echo "IMPORTANT:"
echo "Docker group membership was updated for:"
echo "  ${REAL_USER}"
echo ""
echo "Log out and back in, or run:"
echo "  newgrp docker"
echo "Next steps:"
echo "  - For a full clean-slate build (clears old desktops & database):"
echo "      ./scripts/linux/rebuild.sh"
echo ""
echo "  - To open Flowera (retaining existing desktops and data):"
echo "      ./scripts/linux/build.sh"
echo ""
echo "========================================"