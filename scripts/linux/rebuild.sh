#!/bin/bash
#
# Rebuilds Flowera from a clean slate: no leftover desktop_* containers or
# volumes, and an EMPTY app database (flowera-data: task history, chat
# transcripts, query notes, worker memory). Images are still rebuilt (so a
# change to agent/ or a Dockerfile is picked up), but with Docker's normal
# build cache, not --no-cache - a rebuild should be thorough about clearing
# out data, not slow. This is the "reinstall" script - for "just start what
# I already have", use scripts/linux/build.sh instead.
#
# What is deliberately NOT wiped:
#   - .env (your configuration - API keys, OLLAMA_URL/MODEL, etc.)
#   - the flowera_ollama volume (downloaded models - re-pulling a chat model
#     is a multi-GB download nobody wants on every rebuild)
#
# Flags:
#   -y, --yes   skip the confirmation prompt (for scripted/CI use)
#   --cli       skip the tunnel and open the terminal CLI instead (see
#               `docker compose exec desktop-manager ./flowera --cli`, below)

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# shellcheck source=lib/ollama-setup.sh
source ./scripts/linux/lib/ollama-setup.sh

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

# However far the rebuild got, an interrupted one should not leave a stack
# half old, half fresh - tear down whatever is currently there rather than
# leave it running detached.
cleanup() {
    echo ""
    echo "=== Interrupted: stopping Flowera and its containers ==="

    docker compose down || true

    docker ps -a \
        --filter "name=desktop_" \
        --format "{{.ID}}" \
        | xargs -r docker rm -f

    docker rm -f flowera-ollama >/dev/null 2>&1 || true
}

trap 'cleanup; exit 130' INT TERM

# A fresh clone has no .env at all, and "docker compose up --build" fails
# without one. Start from the example so OLLAMA_MODEL and friends are there.
# An existing .env is left exactly as it is - a rebuild is not a reset of
# your configuration.
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

docker ps -a \
    --filter "name=desktop_" \
    --format "{{.ID}}" \
    | xargs -r docker rm -f

echo ""
echo "=== Removing old desktop volumes ==="

docker volume ls \
    --filter "name=desktop_" \
    --format "{{.Name}}" \
    | xargs -r docker volume rm

echo ""
echo "=== Wiping the app database (flowera-data) ==="

docker volume rm flowera-data >/dev/null 2>&1 \
    || echo "(nothing to remove)"

echo ""
echo "=== Removing the bundled Ollama container (keeping its model volume) ==="

docker rm -f flowera-ollama >/dev/null 2>&1 \
    || echo "(nothing to remove)"

echo ""
./scripts/linux/build-images.sh

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

# The rebuild itself is done at this point (fresh images, fresh desktops,
# empty database, dashboard responding) - clear the interrupt trap before
# handing off to the CLI so leaving it (however that happens) just leaves
# Flowera running, the same as leaving build.sh's CLI does, rather than
# tearing down the stack this script just finished rebuilding.
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
    echo "ERROR: cloudflared is not installed."
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
