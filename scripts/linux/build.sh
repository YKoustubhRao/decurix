#!/bin/bash
#
# Opens Flowera: builds whichever desktop images don't exist yet, makes sure
# a desktop can reach Ollama, starts the control plane if it isn't already
# running, and opens a Cloudflare tunnel to it. Existing desktops, their
# volumes and the app database are left exactly as they are - this is meant
# to feel like reopening an app, not reinstalling it.
#
# Want a clean slate instead (fresh images, fresh desktops, empty database)?
# Use scripts/linux/rebuild.sh.

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# shellcheck source=lib/ollama-setup.sh
source ./scripts/linux/lib/ollama-setup.sh

# Ctrl+C lands here while the script is tailing the tunnel's logs, which run
# forever until interrupted. Unlike rebuild.sh, this script's whole point is
# to leave Flowera running - so interrupting it just stops watching the
# tunnel, it does not tear anything down.
cleanup() {
    echo ""
    echo "=== Tunnel stopped. Flowera is still running: http://127.0.0.1:8080 ==="
    echo "(Run this script again for a new tunnel, or 'docker compose down' to stop Flowera.)"
}

trap 'cleanup; exit 130' INT TERM

echo "========================================"
echo " Opening Flowera"
echo "========================================"

# A fresh clone has no .env at all, and "docker compose up" fails without
# one. Start from the example so OLLAMA_MODEL and friends are there.
if [ ! -f .env ]; then
    if [ -f .env.example ]; then
        echo "No .env yet - creating it from .env.example."
        cp .env.example .env
    else
        touch .env
    fi
fi

# Only build an image that doesn't exist yet - that is the entire difference
# between "open the app" and "rebuild the app". A missing image (first run,
# or a profile added since) still gets built.
./scripts/linux/build-images.sh --only-missing

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

ensure_ollama_ready

echo ""
echo "=== Starting Flowera ==="

# No --build: an already-built control plane image starts as-is. Existing
# desktop_* containers and volumes, and the flowera-data volume (task
# history, chats, worker memory), are untouched.
docker compose up -d

echo ""
echo "=== Waiting for dashboard ==="

for i in {1..60}; do
    if curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
        echo "Flowera dashboard is running."
        break
    fi

    echo "Waiting for Flowera... ($i/60)"
    sleep 2
done

if ! curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
    echo "ERROR: Flowera dashboard is not responding on port 8080."
    docker compose logs --tail=100
    exit 1
fi

echo ""
echo "========================================"
echo " Flowera is running"
echo "========================================"

echo ""
echo "=== Starting Cloudflare Tunnel ==="

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "ERROR: cloudflared is not installed."
    echo "Flowera is still running locally at http://127.0.0.1:8080."
    exit 1
fi

echo ""

# Start Cloudflare tunnel, echoing its logs while watching for the
# trycloudflare.com URL so it can be reported to the control plane for
# display in the dashboard's Mobile dropdown.
cloudflared tunnel --protocol http2 --url http://127.0.0.1:8080 2>&1 | while IFS= read -r line; do
    echo "$line"

    url=$(echo "$line" | grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' || true)

    if [ -n "$url" ]; then
        curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
            -H "Content-Type: application/json" \
            -d "{\"url\":\"$url\"}" \
            >/dev/null 2>&1 || true
    fi
done
