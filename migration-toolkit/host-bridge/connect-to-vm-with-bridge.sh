#!/usr/bin/env bash
# connect-to-vm-with-bridge.sh
# Universal connector: works on macOS, Linux, and Windows (Git Bash / WSL / Cygwin).
# Opens an interactive SSH session to your Linux VM while forwarding a reverse
# tunnel so the Linux VM can execute your local host's apps and access files.

set -euo pipefail

VM_HOST="${1:-}"
VM_PORT="${2:-22}"

if [ -z "$VM_HOST" ]; then
    echo "Usage: $0 <user@linux-vm-ip> [port]"
    exit 1
fi

echo "Connecting to ${VM_HOST}:${VM_PORT} with Universal Host Bridge (reverse port 2222)..."
ssh -p "${VM_PORT}" -R 2222:localhost:22 "${VM_HOST}"

