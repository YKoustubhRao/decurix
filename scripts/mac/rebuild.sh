#!/bin/bash
#
# Rebuilds Decurix from a clean slate on macOS: no leftover desktop_*
# containers or volumes, and an EMPTY app database (decurix-data: task
# history, chat transcripts, query notes, worker memory). Images are
# rebuilt with Docker's build cache. This is the "reinstall" script -
# for "just start what I already have", use scripts/mac/build.sh instead.
#
# What is deliberately NOT wiped:
#   - .env (your configuration - API keys, OLLAMA_URL/MODEL, etc.)
#   - the decurix_ollama volume (downloaded models)
#
# Flags:
#   -y, --yes   skip the confirmation prompt (for scripted/CI use)
#   --cli       skip the tunnel and open the terminal CLI instead

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# shellcheck source=lib/ollama-setup.sh
source ./scripts/mac/lib/ollama-setup.sh
# shellcheck source=../linux/lib/lan-tunnel.sh
source ./scripts/linux/lib/lan-tunnel.sh
# shellcheck source=lib/keep-awake.sh
source ./scripts/mac/lib/keep-awake.sh
# shellcheck source=lib/host-mounts.sh
source ./scripts/mac/lib/host-mounts.sh

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
echo " Decurix rebuild (clean slate)"
echo "========================================"
echo ""
echo "This will permanently delete:"
echo "  - every desktop_* container and volume"
echo "  - the decurix-data volume: ALL task history, chats, query notes and"
echo "    worker memory - this cannot be undone"
echo ""
echo "Kept as-is: .env, and the decurix_ollama volume (your pulled models)."
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
    stop_keep_awake
    echo "=== Interrupted: stopping Decurix and its containers ==="

    docker compose down || true

    local containers
    containers=$(docker ps -a --filter "label=desktop-manager=true" --format "{{.ID}}")
    if [ -n "$containers" ]; then
        echo "$containers" | xargs docker rm -f 2>/dev/null || true
    fi

    docker rm -f decurix-ollama >/dev/null 2>&1 || true
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
echo "=== Stopping Decurix ==="

docker compose down

echo ""
echo "=== Removing old desktop containers ==="

containers=$(docker ps -a --filter "label=desktop-manager=true" --format "{{.ID}}")
if [ -n "$containers" ]; then
    echo "$containers" | xargs docker rm -f
fi

echo ""
echo "=== Removing old desktop volumes ==="

vols=$(docker volume ls --filter "name=^desktop_" --format "{{.Name}}")
if [ -n "$vols" ]; then
    echo "$vols" | xargs docker volume rm
fi

echo ""
echo "=== Wiping the app database (decurix-data) ==="

docker volume rm decurix-data >/dev/null 2>&1 \
    || echo "(nothing to remove)"

echo ""
echo "=== Removing the bundled Ollama container (keeping its model volume) ==="

docker rm -f decurix-ollama >/dev/null 2>&1 \
    || echo "(nothing to remove)"

export DOCKER_BUILDKIT=1

echo ""
./scripts/mac/build-images.sh

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

ensure_ollama_ready

echo ""
echo "=== Starting Decurix ==="

export_host_mounts
docker compose -f docker-compose.yaml ${DECURIX_COMPOSE_OVERRIDE:+-f "$DECURIX_COMPOSE_OVERRIDE"} up -d --build

echo ""
echo "=== Waiting for dashboard ==="

for i in {1..60}; do
    if curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
        echo "Decurix dashboard is running."
        break
    fi

    echo "Waiting for Decurix... ($i/60)"
    sleep 2
done

if ! curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
    echo "ERROR: Decurix dashboard is not responding on port 8080."
    docker compose logs --tail=100
    exit 1
fi

echo ""
echo "========================================"
echo " Decurix is running (clean slate)"
echo "========================================"
echo ""
echo "Idle desktops stop automatically after 5 minutes (set DECURIX_IDLE_STOP_MINUTES=0 in .env to disable, or to another number of minutes)."

lan_port="$(read_env_var DECURIX_LAN_PORT)"
lan_port="${lan_port:-8090}"

lan_ip="$(python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.connect(("1.1.1.1", 80)); print(s.getsockname()[0]); s.close()' 2>/dev/null || true)"
if [ -z "$lan_ip" ]; then
    lan_ip="$(ipconfig getifaddr en0 2>/dev/null || true)"
fi

report_lan_access "$lan_ip" "$lan_port"

# Nothing runs while this machine sleeps - no email trigger, no schedule -
# and the control plane, in its container, cannot stop the host sleeping.
# Held only while the dashboard has something that needs it (lib/keep-awake.sh).
echo ""
start_keep_awake

if [ -n "$CLI_MODE" ]; then
    trap - INT TERM

    echo ""
    echo "=== Opening the Decurix CLI ==="
    echo "(Decurix keeps running after you exit; 'exit' or Ctrl+D to leave.)"
    echo ""

    docker compose exec desktop-manager ./decurix --cli
    cli_status=$?
    stop_keep_awake
    exit $cli_status
fi

echo ""
echo "=== Starting Cloudflare Tunnel ==="

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "cloudflared is not installed (tunnel skipped)."
    echo "Decurix is still running locally at http://127.0.0.1:8080."
    echo "Install via: brew install cloudflared"
    while true; do sleep 3600; done
fi

start_lan_tunnel "$lan_port"

echo ""

# The primary listener (:8080) is deliberately never tunnelled: it has no
# login and can create desktops and run commands on this machine. Only the
# access-code-gated listener above is reachable from off this machine. The
# tunnel to it runs in the background (linux/lib/lan-tunnel.sh, which restarts it
# if it drops); this just keeps the script - and so Ctrl+C handling - in the
# foreground. "sleep & wait" so the trap runs at once, not after the sleep.
while true; do
    sleep 5 &
    wait $!
done

