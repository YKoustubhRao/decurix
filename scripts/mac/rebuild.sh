#!/bin/bash
#
# Rebuilds Flowera from a clean slate on macOS: no leftover desktop_*
# containers or volumes, and an EMPTY app database (flowera-data: task
# history, chat transcripts, query notes, worker memory). Images are
# rebuilt with Docker's build cache. This is the "reinstall" script -
# for "just start what I already have", use scripts/mac/build.sh instead.
#
# What is deliberately NOT wiped:
#   - .env (your configuration - API keys, OLLAMA_URL/MODEL, etc.)
#   - the flowera_ollama volume (downloaded models)
#
# Flags:
#   -y, --yes   skip the confirmation prompt (for scripted/CI use)
#   --cli       skip the tunnel and open the terminal CLI instead

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# shellcheck source=lib/ollama-setup.sh
source ./scripts/mac/lib/ollama-setup.sh
# shellcheck source=lib/lan-tunnel.sh
source ./scripts/mac/lib/lan-tunnel.sh

SKIP_CONFIRM=""
CLI_MODE=""

for arg in "$@"; do
    case "$arg" in
        -y|--yes) SKIP_CONFIRM="1" ;;
        --cli) CLI_MODE="1" ;;
        *)
            echo "ERROR: unknown flag: $arg" >&2
            exit 1
            ;;
    esac
done

echo "========================================"
echo " Flowera rebuild (clean slate)"
echo "========================================"
echo ""
echo "This will permanently delete:"
echo "  - every desktop_* container and volume"
echo "  - the flowera-data volume: ALL task history, chats, query notes and"
echo "    worker memory - this cannot be undone"
echo ""
echo "Kept as-is: .env, and the flowera_ollama volume (your pulled models)."
echo ""

if [ -z "$SKIP_CONFIRM" ]; then
    if [ ! -t 0 ]; then
        echo "ERROR: not running interactively. Re-run with --yes to confirm." >&2
        exit 1
    fi

    read -r -p "Type 'yes' to continue: " confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted. Nothing was changed."
        exit 1
    fi
fi

cleanup() {
    echo ""
    stop_lan_tunnel
    echo "=== Interrupted: stopping Flowera and its containers ==="

    docker compose down || true

    local containers
    containers=$(docker ps -a --filter "name=desktop_" --format "{{.ID}}")
    if [ -n "$containers" ]; then
        echo "$containers" | xargs docker rm -f 2>/dev/null || true
    fi

    docker rm -f flowera-ollama >/dev/null 2>&1 || true
}

trap 'cleanup; exit 130' INT TERM

if [ ! -f .env ]; then
    if [ -f .env.example ]; then
        echo "No .env yet - creating it from .env.example."
        cp .env.example .env
    else
        touch .env
    fi
fi

echo ""
echo "=== Stopping Flowera ==="

docker compose down

echo ""
echo "=== Removing old desktop containers ==="

containers=$(docker ps -a --filter "name=desktop_" --format "{{.ID}}")
if [ -n "$containers" ]; then
    echo "$containers" | xargs docker rm -f
fi

echo ""
echo "=== Removing old desktop volumes ==="

vols=$(docker volume ls --filter "name=desktop_" --format "{{.Name}}")
if [ -n "$vols" ]; then
    echo "$vols" | xargs docker volume rm
fi

echo ""
echo "=== Wiping the app database (flowera-data) ==="

docker volume rm flowera-data >/dev/null 2>&1 \
    || echo "(nothing to remove)"

echo ""
echo "=== Removing the bundled Ollama container (keeping its model volume) ==="

docker rm -f flowera-ollama >/dev/null 2>&1 \
    || echo "(nothing to remove)"

export DOCKER_BUILDKIT=1

echo ""
./scripts/mac/build-images.sh

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

ensure_ollama_ready

echo ""
echo "=== Starting Flowera ==="

docker compose up -d --build

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
echo " Flowera is running (clean slate)"
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
    echo "cloudflared is not installed (tunnel skipped)."
    echo "Flowera is still running locally at http://127.0.0.1:8080."
    echo "Install via: brew install cloudflared"
    while true; do sleep 3600; done
fi

start_lan_tunnel "$lan_port"

echo ""

while true; do
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

    echo "Cloudflare tunnel disconnected. Reconnecting in 3s..."
    sleep 3
done

