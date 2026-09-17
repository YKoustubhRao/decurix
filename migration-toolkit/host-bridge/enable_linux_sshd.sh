#!/usr/bin/env bash
# enable_linux_sshd.sh
# Enables OpenSSH server on a Linux host desktop

set -euo pipefail

echo "======================================================"
echo "   Linux Desktop OpenSSH Server Activation            "
echo "======================================================"

if command -v systemctl >/dev/null 2>&1; then
    sudo systemctl enable --now ssh 2>/dev/null || sudo systemctl enable --now sshd 2>/dev/null || true
    echo "SSH server service enabled on Linux desktop!"
elif command -v service >/dev/null 2>&1; then
    sudo service ssh start || true
fi

