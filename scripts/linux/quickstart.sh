#!/bin/bash
#
# One-shot: takes a fresh Linux checkout all the way to a running Flowera
# dashboard. Chains the two steps README.md documents as separate manual
# steps - scripts/linux/setup.sh (host dependencies, needs root, skipped if
# Docker is already installed) and scripts/linux/build.sh (build any missing
# desktop images, start the control plane if it isn't already running, open
# a Cloudflare tunnel) - into a single command for a first-time setup.
#
# Already have Docker? This just runs build.sh, same as opening the app on
# any later run. Want a clean slate instead (fresh images, fresh desktops,
# empty database)? Use scripts/linux/rebuild.sh directly.

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

echo "========================================"
echo " Flowera quickstart"
echo "========================================"
echo ""

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    echo "Docker is already installed and running - skipping host setup."
else
    echo "Docker not found or not running - running host setup (needs root)..."
    echo ""
    sudo ./scripts/linux/setup.sh

    echo ""
    echo "Host setup installed Docker and added you to the docker group."
    echo "That group membership only takes effect in a NEW shell session."
    echo "Re-run this script:"
    echo "  newgrp docker"
    echo "  ./scripts/linux/quickstart.sh"
    exit 0
fi

echo ""
./scripts/linux/build.sh
