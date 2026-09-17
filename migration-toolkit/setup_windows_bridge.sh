#!/usr/bin/env bash
# ==============================================================================
# setup_windows_bridge.sh (Compatibility Forwarder)
# Delegates to setup_host_bridge.sh which provides universal Windows, macOS,
# and Linux host bridging (win-run, host-run, win-mount, host-clip).
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Ensure legacy symlink exists if needed
if [ ! -e "${SCRIPT_DIR}/windows-bridge" ]; then
    ln -s "${SCRIPT_DIR}/host-bridge" "${SCRIPT_DIR}/windows-bridge"
fi

exec "${SCRIPT_DIR}/setup_host_bridge.sh" "$@"
