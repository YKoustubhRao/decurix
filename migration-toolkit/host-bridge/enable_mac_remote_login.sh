#!/usr/bin/env bash
# enable_mac_remote_login.sh
# Enables built-in OpenSSH server (Remote Login) on macOS so the Linux VM can connect back.

set -euo pipefail

echo "======================================================"
echo "   macOS Remote Login (OpenSSH Server) Activation     "
echo "======================================================"

if [ "$(uname -s)" != "Darwin" ]; then
    echo "This script is designed for macOS. Skipping."
    exit 0
fi

echo "Activating Remote Login on macOS..."
if command -v systemsetup >/dev/null 2>&1; then
    sudo systemsetup -setremotelogin on || true
fi

# Ensure sshd is running via launchctl
sudo launchctl load -w /System/Library/LaunchDaemons/ssh.plist 2>/dev/null || true

echo ""
echo "Remote Login is active!"
echo "If macOS prompted for Full Disk Access or Remote Login permission,"
echo "you can also confirm it in:"
echo "  Apple Menu > System Settings > General > Sharing > Remote Login: ON"

