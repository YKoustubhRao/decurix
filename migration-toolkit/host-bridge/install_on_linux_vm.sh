#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Installing Universal Host Bridge utilities to /usr/local/bin/..."
sudo cp "${SCRIPT_DIR}/host-run" /usr/local/bin/host-run
sudo cp "${SCRIPT_DIR}/host-open" /usr/local/bin/host-open
sudo cp "${SCRIPT_DIR}/host-clip" /usr/local/bin/host-clip
sudo cp "${SCRIPT_DIR}/host-mount" /usr/local/bin/host-mount

# Create cross-platform alias symlinks
sudo ln -sf /usr/local/bin/host-run /usr/local/bin/win-run
sudo ln -sf /usr/local/bin/host-run /usr/local/bin/mac-run
sudo ln -sf /usr/local/bin/host-mount /usr/local/bin/win-mount
sudo ln -sf /usr/local/bin/host-mount /usr/local/bin/mac-mount

sudo chmod +x /usr/local/bin/host-* /usr/local/bin/win-* /usr/local/bin/mac-*

# Install sshfs and netcat for bridging if available on the Linux VM
if command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update -y >/dev/null 2>&1 || true
    sudo apt-get install -y --no-install-recommends sshfs netcat-openbsd >/dev/null 2>&1 || true
fi

echo -e "\033[32m✓ Universal Host Bridge installed on Linux VM!\033[0m"
echo "Available commands on this VM:"
echo "  • host-run <cmd>  (Runs command on local host Mac / Windows / Linux)"
echo "  • host-open <url> (Opens URL or document on local host screen)"
echo "  • host-clip       (Pipes stdin to local host clipboard - pbcopy / clip.exe)"
echo "  • host-mount      (Mounts local host filesystem to /mnt/host via SSHFS with zero duplication)"
echo "  • win-run / mac-run (OS-specific aliases)"

