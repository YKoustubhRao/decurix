#!/bin/bash
#
# Opens Flowera on macOS: builds whichever desktop images don't exist yet,
# makes sure a desktop can reach Ollama, starts the control plane if it
# isn't already running, and opens a Cloudflare tunnel to it. Existing
# desktops, their volumes and the app database are left exactly as they
# are - this is meant to feel like reopening an app, not reinstalling it.
#
# Want a clean slate instead (fresh images, fresh desktops, empty database)?
# Use scripts/mac/rebuild.sh.
#
# Flags:
#   --cli   skip the tunnel and open the terminal CLI instead

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# shellcheck source=lib/ollama-setup.sh
source ./scripts/mac/lib/ollama-setup.sh
# shellcheck source=lib/lan-tunnel.sh
source ./scripts/mac/lib/lan-tunnel.sh

CLI_MODE=""

for arg in "$@"; do
    case "$arg" in
        --cli) CLI_MODE="1" ;;
        *)
            echo "ERROR: unknown flag: $arg" >&2
            exit 1
            ;;
    esac
done

cleanup() {
    echo ""
    stop_lan_tunnel
    echo "=== Tunnel stopped. Flowera is still running: http://127.0.0.1:8080 ==="
    echo "(Run this script again for a new tunnel, or 'docker compose down' to stop Flowera.)"
}

trap 'cleanup; exit 130' INT TERM

echo "========================================"
echo " Opening Flowera (macOS)"
echo "========================================"

if [ ! -f .env ]; then
    if [ -f .env.example ]; then
        echo "No .env yet - creating it from .env.example."
        cp .env.example .env
    else
        touch .env
    fi
fi

# Only build an image that doesn't exist yet - that is the entire difference
# between "open the app" and "rebuild the app".
./scripts/mac/build-images.sh --only-missing

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

ensure_ollama_ready

echo ""
echo "=== Starting Flowera ==="

# No --build: existing desktop_* containers and volumes, and the flowera-data
# volume (task history, chats, worker memory), are untouched.
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
echo "Idle desktops stop automatically after 5 minutes (set FLOWERA_IDLE_STOP_MINUTES=0 in .env to disable, or to another number of minutes)."

lan_port="$(read_env_var FLOWERA_LAN_PORT)"
lan_port="${lan_port:-8090}"

lan_ip="$(python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.connect(("1.1.1.1", 80)); print(s.getsockname()[0]); s.close()' 2>/dev/null || true)"
if [ -z "$lan_ip" ]; then
    lan_ip="$(ipconfig getifaddr en0 2>/dev/null || true)"
fi

if [ -n "$lan_ip" ]; then
    admin_url="http://$lan_ip:8080"
    lan_url="http://$lan_ip:$lan_port"

    curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
        -H "Content-Type: application/json" \
        -d "{\"admin_url\":\"$admin_url\",\"lan_url\":\"$lan_url\"}" \
        >/dev/null 2>&1 || true

    echo ""
    echo "Admin URL (full control, no code - keep this to yourself): $admin_url"
    echo "LAN access URL (needs the code below, works without the tunnel):  $lan_url"
fi

if command -v jq >/dev/null 2>&1; then
    lan_code="$(curl -fsS http://127.0.0.1:8080/api/lan-access/status 2>/dev/null | jq -r '.code // empty')"

    if [ -n "$lan_code" ]; then
        echo ""
        echo "Access code: $lan_code"
        echo "(Needed once by anyone using the LAN URL above. Also in the dashboard's Mobile menu, which can generate a new one.)"

        if [ -n "$lan_url" ]; then
            echo ""
            echo "Open it yourself without typing the code: $lan_url/__flowera_access/authenticate?code=$lan_code"
        fi
    fi
fi

if [ -n "$CLI_MODE" ]; then
    trap - INT TERM

    echo ""
    echo "=== Opening the Flowera CLI ==="
    echo "(Flowera keeps running after you exit; 'exit' or Ctrl+D to leave.)"
    echo ""

    docker compose exec desktop-manager ./flowera --cli
    exit $?
fi

echo ""
echo "=== Starting Cloudflare Tunnel ==="

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "cloudflared is not installed (optional quick tunnel skipped)."
    echo "Flowera is still running locally at http://127.0.0.1:8080."
    echo "Install via: brew install cloudflared"
    echo "Press Ctrl+C to stop Flowera."
    while true; do sleep 3600; done
fi

start_lan_tunnel "$lan_port"

echo ""

cloudflared tunnel --protocol http2 --url "http://127.0.0.1:8080" 2>&1 | while IFS= read -r line; do
    echo "$line"

    url=$(echo "$line" | grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' || true)

    if [ -n "$url" ]; then
        curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
            -H "Content-Type: application/json" \
            -d "{\"url\":\"$url\"}" \
            >/dev/null 2>&1 || true

        echo "(No access code needed for this URL - full control of this machine. Keep it to yourself.)"
    fi
done
